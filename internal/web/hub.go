package web

import (
	"encoding/json"
	"sync"
	"time"
)

// Replay sizes: a page that connects (or reloads) gets the events of the
// current task plus a little context before it, so it can rebuild the
// agent tree and the log.
const (
	maxReplay      = 8000
	keepBeforeTask = 300
	clientBuffer   = 4096
)

// hub fans server-sent events out to every connected page and keeps the
// replay buffer.
type hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}
	replay  [][]byte

	// Connection bookkeeping for `sy app` (exit when the window is gone).
	everConnected bool
	lastSeen      time.Time
}

type client struct {
	ch     chan []byte
	closed bool
}

func newHub() *hub { return &hub{clients: map[*client]struct{}{}} }

// frame encodes one SSE message.
func frame(name string, v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		data = []byte(`{}`)
	}
	b := make([]byte, 0, len(data)+len(name)+16)
	b = append(b, "event: "...)
	b = append(b, name...)
	b = append(b, "\ndata: "...)
	b = append(b, data...)
	b = append(b, "\n\n"...)
	return b
}

// publish sends a frame to every client; record also keeps it for replay.
// A client that cannot keep up is dropped (its page reconnects and gets
// the replay), so a slow tab never blocks the orchestrator.
func (h *hub) publish(f []byte, record, taskStart bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if record {
		if taskStart && len(h.replay) > keepBeforeTask {
			h.replay = append([][]byte(nil), h.replay[len(h.replay)-keepBeforeTask:]...)
		}
		h.replay = append(h.replay, f)
		if len(h.replay) > maxReplay {
			h.replay = append([][]byte(nil), h.replay[len(h.replay)-maxReplay*3/4:]...)
		}
	}
	for c := range h.clients {
		select {
		case c.ch <- f:
		default:
			h.dropLocked(c)
		}
	}
}

// subscribe registers a client and returns the replay to send first.
func (h *hub) subscribe() (*client, [][]byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := &client{ch: make(chan []byte, clientBuffer)}
	h.clients[c] = struct{}{}
	h.everConnected = true
	h.lastSeen = time.Now()
	return c, append([][]byte(nil), h.replay...)
}

func (h *hub) unsubscribe(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(c)
	h.lastSeen = time.Now()
}

func (h *hub) dropLocked(c *client) {
	if c.closed {
		return
	}
	c.closed = true
	delete(h.clients, c)
	close(c.ch)
}

// connections reports the number of connected pages, whether one ever
// connected, and when the last one left (or connected).
func (h *hub) connections() (n int, ever bool, last time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients), h.everConnected, h.lastSeen
}

// closeAll ends every stream (shutdown).
func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		h.dropLocked(c)
	}
}
