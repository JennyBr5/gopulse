package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetState resets package globals between tests to avoid cross-test interference.
func resetState() {
	// Stop tracing if running
	_ = Stop()

	// Reset the atomic and globals
	started.Store(false)
	wmu.Lock()
	writer = nil
	if outFile != nil && outFile != os.Stdout {
		_ = outFile.Close()
	}
	outFile = nil
	wmu.Unlock()

	// Clear maps and the buffers
	gm.Range(func(k, _ any) bool { gm.Delete(k); return true })
	starts.Range(func(k, _ any) bool { starts.Delete(k); return true })
	stops.Range(func(k, _ any) bool { stops.Delete(k); return true })

	bmu.Lock()
	for ch := range subscribers {
		delete(subscribers, ch)
		close(ch)
	}
	subscribers = nil
	buffer = nil
	bmu.Unlock()

	lastEventTS.Store(nil)

	// Reset counters
	gidCounter.Store(0)
}

// readEvents loads the JSONL events from a file and returns  as a slice.
func readEvents(t *testing.T, path string) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal event: %v; line=%s", err, line)
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan events: %v", err)
	}
	return events
}

func TestStartGoStop_EmitsEvents(t *testing.T) {
	resetState()

	dir := t.TempDir()
	out := filepath.Join(dir, "events.jsonl")

	// Make deterministic timestamps
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := int64(0)
	oldNow := now
	now = func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Millisecond) }
	defer func() { now = oldNow }()

	if err := Start(Config{Output: out}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	Go(func() { defer wg.Done() })
	wg.Wait()

	if err := Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	events := readEvents(t, out)
	if len(events) < 3 {
		t.Fatalf("expected at least 3 events, got %d", len(events))
	}

	// Expect types to include start, g_start, g_stop in that order. Note: Stop() does not emit a 'stop' event because emit is gated by started flag.
	gotTypes := make([]string, 0, len(events))
	for _, e := range events {
		gotTypes = append(gotTypes, e.Type)
	}

	// Find indices
	idx := func(s string) int {
		for i, v := range gotTypes {
			if v == s {
				return i
			}
		}
		return -1
	}

	iStart := idx("start")
	iGStart := idx(EventGStart)
	iGStop := idx(EventGStop)

	if iStart < 0 || iGStart < 0 || iGStop < 0 {
		t.Fatalf("missing expected event types; got %v", gotTypes)
	}
	if !(iStart < iGStart && iGStart < iGStop) {
		t.Fatalf("unexpected order: %v", gotTypes)
	}
}

func TestChan_SendRecvClose_EmitsChannelEvents(t *testing.T) {
	resetState()

	dir := t.TempDir()
	out := filepath.Join(dir, "chan.jsonl")

	if err := Start(Config{Output: out}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ch := NewChan[int](0, "test-chan")

	var wg sync.WaitGroup
	wg.Add(1)
	Go(func() {
		defer wg.Done()
		ch.Send(42)
	})

	// Receive and close on main goroutine
	if v, ok := ch.Recv(); !ok || v != 42 {
		t.Fatalf("unexpected recv: v=%d ok=%v", v, ok)
	}
	ch.Close()

	wg.Wait()

	if err := Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	events := readEvents(t, out)
	// Collect channel event types for our channel id
	var types []string
	for _, e := range events {
		if e.ChanID == ch.ID() {
			if e.Type == EventCSend || e.Type == EventCRecv || e.Type == EventCClose {
				types = append(types, e.Type)
			}
		}
	}
	if len(types) != 3 {
		t.Fatalf("expected 3 channel events, got %d (%v)", len(types), types)
	}
	// Recv is emitted before the actual receive operation, so it may appear before send; close should be last
	idx := map[string]int{}
	for i, tpe := range types {
		idx[tpe] = i
	}
	if !(idx[EventCRecv] < idx[EventCSend] && idx[EventCSend] < idx[EventCClose]) {
		t.Fatalf("unexpected channel event order: %v", types)
	}
}

func TestBlockUnblock_EmitsEventsWithReason(t *testing.T) {
	resetState()

	dir := t.TempDir()
	out := filepath.Join(dir, "block.jsonl")

	if err := Start(Config{Output: out}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	gid := LogicalGID()
	done := Block("db-query")
	// simulate some work
	time.Sleep(5 * time.Millisecond)
	done()

	if err := Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	events := readEvents(t, out)
	var sawBlock, sawUnblock bool
	for _, e := range events {
		if e.GID != gid {
			continue
		}
		switch e.Type {
		case EventBlock:
			var d map[string]any
			_ = json.Unmarshal(e.Details, &d)
			if d["reason"] != "db-query" {
				t.Fatalf("expected reason 'db-query', got %v", d["reason"])
			}
			sawBlock = true
		case EventUnblock:
			sawUnblock = true
		}
	}
	if !sawBlock || !sawUnblock {
		t.Fatalf("expected block and unblock events for gid=%d", gid)
	}
}

func TestLogicalGID_StableOnSameGoroutine(t *testing.T) {
	resetState()

	// LogicalGID should be stable within the same goroutine
	g1 := LogicalGID()
	g2 := LogicalGID()
	if g1 != g2 {
		t.Fatalf("LogicalGID not stable: %d vs %d", g1, g2)
	}

	var wg sync.WaitGroup
	var childID int64
	wg.Add(1)
	Go(func() {
		defer wg.Done()
		childID = LogicalGID()
	})
	wg.Wait()

	if childID == 0 {
		t.Fatalf("child LogicalGID not set")
	}
	if childID == g1 {
		t.Fatalf("expected different gid for child; main=%d child=%d (runtime: %d)", g1, childID, runtime.NumGoroutine())
	}
}
