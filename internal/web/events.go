package web

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"paperfull/internal/ingest"
)

// Event is pushed to open pages so they can update themselves.
type Event struct {
	Type      string       `json:"type"`                // "task" or "changed"
	Task      *ingest.Task `json:"task,omitempty"`      // for "task"
	Documents []int64      `json:"documents,omitempty"` // for "changed": affected ids, empty = anything
}

// hub fans events out to every connected browser.
type hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func (h *hub) subscribe() chan Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[chan Event]struct{}{}
	}
	c := make(chan Event, 64)
	h.subs[c] = struct{}{}
	return c
}

func (h *hub) unsubscribe(c chan Event) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

func (h *hub) publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		select {
		case c <- e:
		default: // a stuck client misses an event rather than blocking everyone
		}
	}
}

// changed tells pages that documents (or tags, senders, types) changed.
func (s *Server) changed(ids ...int64) {
	s.events.publish(Event{Type: "changed", Documents: ids})
}

func (s *Server) taskEvent(t ingest.Task) {
	s.events.publish(Event{Type: "task", Task: &t})
	switch t.Status {
	case ingest.StatusDone:
		s.changed(t.DocumentID)
	}
}

// apiEvents streams events as server-sent events.
func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // let nginx pass events through immediately
	w.WriteHeader(http.StatusOK)
	// Tell the browser to reconnect quickly if the server restarts.
	w.Write([]byte("retry: 2000\n\n"))
	rc.Flush()

	c := s.events.subscribe()
	defer s.events.unsubscribe(c)
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
		case e := <-c:
			b, _ := json.Marshal(e)
			if _, err := w.Write([]byte("data: " + string(b) + "\n\n")); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// touched is called after anything was edited: retrain soon and let open pages refresh.
func (s *Server) touched() {
	s.Cls.MarkDirty()
	s.changed()
}
