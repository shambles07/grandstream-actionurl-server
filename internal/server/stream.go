package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/shambles07/grandstream-actionurl-server/internal/store"
)

// hub fans stored events out to Server-Sent Events subscribers, so a future
// web UI can update live without polling.
type hub struct {
	mu     sync.Mutex
	subs   map[chan store.Event]struct{}
	closed bool
}

func newHub() *hub { return &hub{subs: map[chan store.Event]struct{}{}} }

func (h *hub) subscribe() (chan store.Event, func()) {
	ch := make(chan store.Event, 64)
	h.mu.Lock()
	if h.closed {
		close(ch)
	} else {
		h.subs[ch] = struct{}{}
	}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// publish never blocks: a subscriber that falls behind misses events.
func (h *hub) publish(e store.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}

// handleStream streams new events as Server-Sent Events. Optional mac= and
// event= query parameters filter the stream.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	mac := normalizeMAC(r.URL.Query().Get("mac"))
	event := r.URL.Query().Get("event")

	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
		case e, ok := <-ch:
			if !ok {
				return
			}
			if (mac != "" && e.MAC != mac) || (event != "" && e.Event != event) {
				continue
			}
			var b bytes.Buffer
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			enc.Encode(e) // appends '\n', which ends the data: line
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n", e.ID, e.Event, b.Bytes())
		}
		flusher.Flush()
	}
}
