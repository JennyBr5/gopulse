package trace

import (
	"encoding/json"
	"time"
)

// EventType enumerates supported event kinds.
const (
	EventGStart   = "g_start"   // goroutine started
	EventGStop    = "g_stop"    // goroutine finished
	EventCSend    = "c_send"    // channel send
	EventCRecv    = "c_recv"    // channel receive
	EventCClose   = "c_close"   // channel close
	EventBlock    = "block"     // potential blocking point
	EventUnblock  = "unblock"   // unblocked/resumed
	EventDeadlock = "deadlock_hint" // emitted when potential deadlock is detected
)

// Event is a single concurrency tracing event. It is JSON-serializable and written as JSON Lines (one per line).
// Keep this struct stable for tooling compatibility.
type Event struct {
	Time    time.Time       `json:"time"`
	Type    string          `json:"type"`
	GID     int64           `json:"gid"`               // Goroutine id (logical id if runtime id unavailable reliably)
	ChanID  string          `json:"chan_id,omitempty"` // Channel id (string to allow generic tracking)
	Details json.RawMessage `json:"details,omitempty"`
}

// Marshal details helper.
func details(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
