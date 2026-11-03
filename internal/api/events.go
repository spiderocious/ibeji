package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Event is something the daemon wants its clients to know about: a device came
// online, a sync converged, a permission prompt is waiting.
type Event struct {
	Type string    `json:"type"`
	Data any       `json:"data,omitempty"`
	At   time.Time `json:"at"`
}

// Event types. Stable strings, like the error codes: clients and agents branch
// on them.
const (
	EventDeviceChanged   = "device.changed"
	EventDeviceOnline    = "device.online"
	EventDeviceOffline   = "device.offline"
	EventDiscovery       = "discovery.changed"
	EventPairingStarted  = "pairing.started"
	EventPairingDone     = "pairing.completed"
	EventKeyMismatch     = "device.key_mismatch"
	EventPromptPending   = "prompt.pending"
	EventPromptResolved  = "prompt.resolved"
	EventSyncState       = "space.state"
	EventSyncConflict    = "space.conflict"
	EventProcessChanged  = "process.changed"
	EventTunnelChanged   = "tunnel.changed"
	EventTransferChanged = "transfer.changed"
)

// eventHub fans one event out to every subscribed client.
type eventHub struct {
	mu     sync.RWMutex
	subs   map[int]chan Event
	nextID int
	closed bool
}

func newEventHub() *eventHub {
	return &eventHub{subs: map[int]chan Event{}}
}

// Publish sends to every subscriber.
//
// A subscriber that is not keeping up is skipped rather than blocked on: the
// daemon holds the link and the watchers, and a window that stopped reading
// must never be able to stall it. The window re-reads state on reconnect, so a
// dropped event costs a refresh, not correctness — events are a nudge to
// re-read, never the only copy of a fact.
func (h *eventHub) Publish(typ string, data any) {
	ev := Event{Type: typ, Data: data, At: time.Now()}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return
	}
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// subscribe registers a client and returns its channel plus a disposer.
func (h *eventHub) subscribe() (<-chan Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	id := h.nextID
	h.nextID++
	// Buffered: a short burst (several devices changing at once) should not be
	// dropped just because the client is mid-render.
	ch := make(chan Event, 32)
	h.subs[id] = ch

	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
	}
}

// Subscribers reports how many clients are listening, for tests and doctor.
func (h *eventHub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

func (h *eventHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, ch := range h.subs {
		delete(h.subs, id)
		close(ch)
	}
}

// HandleEvents serves the SSE stream at GET /v1/events.
func (s *Server) HandleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, Errf(CodeInternal, "streaming is not supported by this server"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, unsubscribe := s.events.subscribe()
	defer unsubscribe()

	// A heartbeat keeps the connection from being reaped by an idle timeout and
	// lets the client notice a dead daemon without waiting for a real event.
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case ev, open := <-events:
			if !open {
				return
			}
			body, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, body); err != nil {
				return
			}
			flusher.Flush()

		case <-heartbeat.C:
			// A comment frame: valid SSE, ignored by clients, keeps the socket
			// warm.
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
