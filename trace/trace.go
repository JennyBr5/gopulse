package trace

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/trace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Package-level state
var (
	started atomic.Bool
	wmu     sync.Mutex
	writer  *bufio.Writer
	outFile *os.File
	now     = time.Now

	gidCounter atomic.Int64 // logical goroutine id counter if runtime.Goid not available
	gm         sync.Map     // map[realGID]logicalID

	// live broadcasting
	bmu         sync.RWMutex
	subscribers map[chan Event]struct{}
	buffer      []Event // ring buffer of recent events
	bufferSize  = 1000
	lastEventTS atomic.Pointer[time.Time]

	// for deadlock detection (simple heuristic)
	starts sync.Map // gid -> struct{}
	stops  sync.Map // gid -> struct{}
)

// Config controls tracing output behavior.
type Config struct {
	// Output can be "stdout" (default) or a filepath to write JSONL events.
	Output string
	// SampleBlocks controls emitting block/unblock hints (default true).
	SampleBlocks bool
}

// Start initializes the tracing system. Safe to call multiple times, subsequent calls are no-ops.
func Start(opts ...Config) error {
	if started.Load() {
		return nil
	}
	cfg := defaultConfig()
	if len(opts) > 0 {
		cfg = mergeConfig(cfg, opts[0])
	}

	w, f, err := makeWriter(cfg.Output)
	if err != nil {
		return err
	}
	wmu.Lock()
	writer = w
	outFile = f
	wmu.Unlock()

	// init broadcaster
	bmu.Lock()
	if subscribers == nil {
		subscribers = make(map[chan Event]struct{})
	}
	if buffer == nil {
		buffer = make([]Event, 0, bufferSize)
	}
	bmu.Unlock()

	started.Store(true)

	// Optionally start runtime/trace if GOROUTINE_VIZ_RUNTIME_TRACE=1
	if os.Getenv("GOPULSE_RUNTIME_TRACE") == "1" {
		_ = trace.Start(ioDiscard{}) // start with dummy writer to enable runtime probes
	}

	// Start deadlock watchdog if enabled
	if os.Getenv("GOPULSE_DETECT_DEADLOCK") == "1" {
		go deadlockWatchdog()
	}

	// Emit an initial heartbeat event (not strictly needed)
	emit(Event{Time: now(), Type: "start", GID: 0})
	return nil
}

// Stop flushes and closes writers. Safe to call without Start.
func Stop() error {
	if !started.Load() {
		return nil
	}
	started.Store(false)
	wmu.Lock()
	defer wmu.Unlock()
	if writer != nil {
		_ = writer.Flush()
	}
	if outFile != nil && outFile != os.Stdout {
		_ = outFile.Close()
	}
	if os.Getenv("GOPULSE_RUNTIME_TRACE") == "1" {
		trace.Stop()
	}
	emit(Event{Time: now(), Type: "stop", GID: 0})
	return nil
}

// Go starts the provided function in a new goroutine and emits start/stop events.
// It returns immediately.
func Go(fn func()) {
	gid := nextGID()
	go func() {
		_real := realGID()
		gm.Store(_real, gid)
		emit(Event{Time: now(), Type: EventGStart, GID: gid, Details: details(map[string]any{"real_gid": _real})})
		defer func() {
			if r := recover(); r != nil {
				emit(Event{Time: now(), Type: EventGStop, GID: gid, Details: details(map[string]any{"panic": fmt.Sprint(r)})})
				panic(r)
			} else {
				emit(Event{Time: now(), Type: EventGStop, GID: gid})
			}
		}()
		fn()
	}()
}

// Block marks a potential blocking point for the current goroutine.
func Block(reason string) func() {
	gid := LogicalGID()
	emit(Event{Time: now(), Type: EventBlock, GID: gid, Details: details(map[string]string{"reason": reason})})
	return func() {
		emit(Event{Time: now(), Type: EventUnblock, GID: gid})
	}
}

// LogicalGID returns the logical id used by this package for the current goroutine.
func LogicalGID() int64 {
	gid := realGID()
	if v, ok := gm.Load(gid); ok {
		return v.(int64)
	}
	// main goroutine or untracked: allocate a stable id for it once
	id := nextGID()
	gm.Store(gid, id)
	return id
}

// Chan is a generic channel wrapper that captures send/recv/close events.
// It embeds an underlying channel and provides methods with instrumentation.
type Chan[T any] struct {
	c      chan T
	id     string
	closed atomic.Bool
}

// NewChan creates an instrumented channel with buffer size n.
func NewChan[T any](n int, name ...string) *Chan[T] {
	var id string
	if len(name) > 0 && name[0] != "" {
		id = name[0]
	} else {
		id = "chan-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return &Chan[T]{c: make(chan T, n), id: id}
}

func (ch *Chan[T]) ID() string { return ch.id }

func (ch *Chan[T]) Send(v T) {
	gid := LogicalGID()
	emit(Event{Time: now(), Type: EventCSend, GID: gid, ChanID: ch.id})
	ch.c <- v
}

func (ch *Chan[T]) Recv() (T, bool) {
	gid := LogicalGID()
	emit(Event{Time: now(), Type: EventCRecv, GID: gid, ChanID: ch.id})
	v, ok := <-ch.c
	return v, ok
}

func (ch *Chan[T]) Close() {
	if ch.closed.Swap(true) {
		return
	}
	close(ch.c)
	emit(Event{Time: now(), Type: EventCClose, GID: LogicalGID(), ChanID: ch.id})
}

func (ch *Chan[T]) Raw() chan T { return ch.c }

// Helpers

func emit(e Event) {
	if !started.Load() {
		return
	}
	// 1) write to configured output (stdout/file)
	b, _ := json.Marshal(e)
	wmu.Lock()
	if writer != nil {
		_, _ = writer.Write(b)
		_, _ = writer.WriteString("\n")
		_ = writer.Flush()
	}
	wmu.Unlock()

	// 2) update last event timestamp
	te := e.Time
	lastEventTS.Store(&te)

	// 3) maintain starts/stops for deadlock/leak hints
	switch e.Type {
	case EventGStart:
		starts.Store(e.GID, struct{}{})
	case EventGStop:
		stops.Store(e.GID, struct{}{})
	}

	// 4) append to ring buffer and broadcast to subscribers
	bmu.Lock()
	// append with cap bound
	if len(buffer) < bufferSize {
		buffer = append(buffer, e)
	} else {
		// drop oldest (simple FIFO): shift left by one and put new at end
		copy(buffer[0:], buffer[1:])
		buffer[len(buffer)-1] = e
	}
	// broadcast non-blocking
	for ch := range subscribers {
		select {
		case ch <- e:
		default:
			// drop if subscriber is slow
		}
	}
	bmu.Unlock()
}

func nextGID() int64 { return gidCounter.Add(1) }

// realGID tries to get the current runtime's goroutine id. Go 1.20+ doesn't expose it officially; here
// we use runtime/trace labels as a stable proxy: we cannot. As a fallback, parse from the stack prefix.
// This is best-effort and only used to map the main goroutine once.
func realGID() int64 {
	// Fallback parsing trick: this is not guaranteed but commonly used in tools.
	var b [64]byte
	n := runtime.Stack(b[:], false)
	// Stack header example: "goroutine 1 [running]:\n"
	for i := 10; i < n; i++ { // after "goroutine "
		if b[i] == ' ' {
			id, _ := strconv.ParseInt(string(b[10:i]), 10, 64)
			return id
		}
	}
	return 0
}

func defaultConfig() Config {
	out := os.Getenv("GOPULSE_OUTPUT")
	if out == "" {
		out = "stdout"
	}
	return Config{Output: out, SampleBlocks: true}
}

func mergeConfig(a, b Config) Config {
	if b.Output != "" {
		a.Output = b.Output
	}
	if !b.SampleBlocks {
		a.SampleBlocks = false
	}
	return a
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func makeWriter(output string) (*bufio.Writer, *os.File, error) {
	if output == "" || output == "stdout" || output == "-" {
		return bufio.NewWriter(os.Stdout), os.Stdout, nil
	}
	// ensure directory exists (if any)
	dir := dirname(output)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, nil, err
		}
	}
	f, err := os.Create(output)
	if err != nil {
		return nil, nil, err
	}
	return bufio.NewWriter(f), f, nil
}

func dirname(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return ""
}

// --- Live UI server (in-process) ---

// UI starts a lightweight HTTP server that serves the live UI at addr (e.g., ":8080" or "127.0.0.1:8080").
// It returns a stop function to gracefully shut it down.
func UI(addr string) (func(context.Context) error, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(liveIndexHTML))
	})
	mux.HandleFunc("/stream", sseHandler)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { _ = srv.ListenAndServe() }()
	return srv.Shutdown, nil
}

func sseHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// send buffer snapshot first
	bmu.RLock()
	snapshot := make([]Event, len(buffer))
	copy(snapshot, buffer)
	bmu.RUnlock()
	for _, e := range snapshot {
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
	}
	fl.Flush()

	// subscribe for live events
	ch := make(chan Event, 256)
	bmu.Lock()
	if subscribers == nil {
		subscribers = make(map[chan Event]struct{})
	}
	subscribers[ch] = struct{}{}
	bmu.Unlock()
	defer func() {
		bmu.Lock()
		delete(subscribers, ch)
		close(ch)
		bmu.Unlock()
	}()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case e := <-ch:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", string(b))
			fl.Flush()
		}
	}
}

// deadlockWatchdog periodically checks for inactivity and incomplete goroutines
func deadlockWatchdog() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	timeout := 5 * time.Second
	for range ticker.C {
		if !started.Load() {
			return
		}
		// last event time
		lt := lastEventTS.Load()
		if lt == nil {
			continue
		}
		if time.Since(*lt) < timeout {
			continue
		}
		// compute leaked gids: in starts but not in stops
		var leaked []int64
		starts.Range(func(key, value any) bool {
			gid := key.(int64)
			if _, ok := stops.Load(gid); !ok {
				leaked = append(leaked, gid)
			}
			return true
		})
		if len(leaked) == 0 {
			continue
		}
		// emit a hint
		emit(Event{Time: now(), Type: EventDeadlock, GID: 0, Details: details(map[string]any{"leaked": leaked, "hint": "No events for a while; goroutines without stop detected"})})
	}
}

// Embedded HTML for live UI (shared by trace.UI and CLI when desired)
const liveIndexHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <title>GoPulse Live UI</title>
  <style>
    body { font-family: -apple-system, Segoe UI, Roboto, sans-serif; margin: 20px; }
    h2 { margin-top: 20px; }
    table { border-collapse: collapse; width: 100%; margin-top: 8px; }
    th, td { border-bottom: 1px solid #eee; padding: 6px 8px; text-align: left; }
    .status-running { color: #2563EB; }
    .status-finished { color: #059669; }
    .status-observed { color: #6B7280; }
    .status-blocked { color: #F59E0B; font-weight: 600; }
    .status-deadlock { color: #DC2626; font-weight: 600; }
    .pill { display:inline-block; padding: 2px 6px; border-radius: 10px; background:#F3F4F6; font-size: 12px; margin-right:4px; }
    .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
    #summary { margin: 12px 0; color: #374151; }
    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 8px; }
    .card { background: #F9FAFB; border: 1px solid #E5E7EB; border-radius: 8px; padding: 10px; }
  </style>
</head>
<body>
  <h1>GoPulse Live</h1>
  <p>Live goroutine and channel activity. This page updates automatically.</p>
  <div id="deadlock" class="status-deadlock" style="display:none;">Potential deadlock detected</div>

  <div id="summary" class="grid">
    <div class="card"><b>Total events:</b> <span id="sum-events">0</span></div>
    <div class="card"><b>Total goroutines:</b> <span id="sum-g">0</span></div>
    <div class="card"><b>Running:</b> <span id="sum-running">0</span></div>
    <div class="card"><b>Blocked:</b> <span id="sum-blocked">0</span></div>
    <div class="card"><b>Finished:</b> <span id="sum-finished">0</span></div>
    <div class="card"><b>Observed:</b> <span id="sum-observed">0</span></div>
  </div>

  <h2>Goroutines</h2>
  <table>
    <thead>
      <tr><th>Goroutine</th><th>Status</th><th>Duration</th><th>Sends</th><th>Receives</th><th>Closes</th><th>Last Event</th></tr>
    </thead>
    <tbody id="tbody"></tbody>
  </table>

  <h2>Channels</h2>
  <table>
    <thead>
      <tr><th>Channel</th><th>Sends</th><th>Receives</th><th>Closes</th></tr>
    </thead>
    <tbody id="chbody"></tbody>
  </table>

  <script>
    const state = new Map(); // gid -> {start, stop, sends, recvs, closes, lastTime, blocked, blockReason}
    const channels = new Map(); // id -> {sends, recvs, closes}
    let totalEvents = 0;

    function upsert(e){
      totalEvents++;
      const gid = e.gid;
      let s = state.get(gid) || {sends:0, recvs:0, closes:0};
      const t = new Date(e.time);
      s.lastTime = t;
      if(e.type==='g_start'){ s.start = t; }
      if(e.type==='g_stop'){ s.stop = t; s.blocked = false; s.blockReason = undefined; }
      if(e.type==='block'){
        s.blocked = true; try{ s.blockReason = (e.details && e.details.reason) || ''; }catch(_){}
      }
      if(e.type==='unblock'){ s.blocked = false; }
      if(e.type==='c_send'){ s.sends++; touchChan(e.chan_id, 'sends'); }
      if(e.type==='c_recv'){ s.recvs++; touchChan(e.chan_id, 'recvs'); }
      if(e.type==='c_close'){ s.closes++; touchChan(e.chan_id, 'closes'); }
      state.set(gid, s);
    }

    function touchChan(id, field){
      if(!id) return;
      let c = channels.get(id) || {sends:0, recvs:0, closes:0};
      c[field]++;
      channels.set(id, c);
    }

    function statusOf(s){
      if(s.blocked) return ['blocked' + (s.blockReason? (' ('+s.blockReason+')') : ''),'status-blocked'];
      if(s.stop) return ['finished','status-finished'];
      if(s.start) return ['running','status-running'];
      return ['observed','status-observed'];
    }

    function render(){
      // Summary
      let running=0, finished=0, observed=0, blocked=0;
      for(const s of state.values()){
        if(s.blocked) blocked++; else if(s.stop) finished++; else if(s.start) running++; else observed++;
      }
      document.getElementById('sum-events').textContent = String(totalEvents);
      document.getElementById('sum-g').textContent = String(state.size);
      document.getElementById('sum-running').textContent = String(running);
      document.getElementById('sum-blocked').textContent = String(blocked);
      document.getElementById('sum-finished').textContent = String(finished);
      document.getElementById('sum-observed').textContent = String(observed);

      // Goroutines table
      const tbody = document.getElementById('tbody');
      tbody.innerHTML = '';
      const gids = Array.from(state.keys()).sort((a,b)=>a-b);
      for(const gid of gids){
        const s = state.get(gid);
        const tr = document.createElement('tr');
        const [label, cls] = statusOf(s);
        const dur = s.start ? ((s.stop||new Date()) - s.start) : 0;
        tr.innerHTML = '<td class="mono">G#'+gid+'</td>'+
          '<td class="'+cls+'">'+label+'</td>'+
          '<td>'+(s.start? (dur/1000).toFixed(3)+'s' : '-')+'</td>'+
          '<td><span class="pill">'+s.sends+'</span></td>'+
          '<td><span class="pill">'+s.recvs+'</span></td>'+
          '<td><span class="pill">'+s.closes+'</span></td>'+
          '<td class="mono">'+(s.lastTime? s.lastTime.toISOString(): '-')+'</td>';
        tbody.appendChild(tr);
      }

      // Channels table
      const chbody = document.getElementById('chbody');
      chbody.innerHTML = '';
      const ids = Array.from(channels.keys()).sort();
      for(const id of ids){
        const c = channels.get(id);
        const tr = document.createElement('tr');
        tr.innerHTML = '<td class="mono">'+id+'</td>'+
          '<td><span class="pill">'+c.sends+'</span></td>'+
          '<td><span class="pill">'+c.recvs+'</span></td>'+
          '<td><span class="pill">'+c.closes+'</span></td>';
        chbody.appendChild(tr);
      }
    }

    function handleEvent(e){
      if(e.type==='deadlock_hint'){
        const dl = document.getElementById('deadlock');
        dl.style.display = 'block';
        dl.textContent = 'Potential deadlock: ' + (e.details? JSON.stringify(e.details): '');
      } else {
        upsert(e);
      }
      render();
    }

    function boot(){
      if (!!window.EventSource){
        const es = new EventSource('/stream');
        es.onmessage = (msg)=>{ try{ handleEvent(JSON.parse(msg.data)); }catch(_){} };
      }
    }
    boot();
  </script>
</body>
</html>`
