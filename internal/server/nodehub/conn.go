package nodehub

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/wsjson"
)

func newID() string { return uuid.NewString() }

func version() string { return buildVersion }

// Version exposes the stamped build version to other packages (e.g.
// the /api/v1/version endpoint).
func Version() string { return buildVersion }

// buildVersion is stamped via -ldflags "-X ...nodehub.buildVersion=..."
// for releases; "dev" otherwise.
var buildVersion = "dev"

// conn is one live agent connection plus its in-flight dispatches.
type conn struct {
	node *store.Node
	ws   *wsjson.Conn
	hub  *Hub

	mu        sync.RWMutex
	pending   map[string]*pendingReq
	lastBeatT time.Time
	latency   float64
}

type pendingReq struct {
	ch   chan contracts.ChatChunk
	done chan struct{}
	once sync.Once
}

func newConn(node *store.Node, ws *wsjson.Conn, h *Hub) *conn {
	return &conn{
		node:      node,
		ws:        ws,
		hub:       h,
		pending:   map[string]*pendingReq{},
		lastBeatT: time.Now(),
	}
}

// send serializes one envelope on the wire. wsjson.Conn.Write is itself
// safe for concurrent use (coder/websocket serializes writers).
func (c *conn) send(ctx context.Context, env contracts.Envelope) error {
	return c.ws.Write(ctx, env)
}

// startPending atomically reserves one execution slot. This is the
// capacity enforcement point: the scheduler's capacity check runs on a
// snapshot and can race concurrent picks, so the configured per-node
// limit is enforced here under the conn mutex — a busy node is never
// oversubscribed.
func (c *conn) startPending(reqID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	max := c.node.MaxConcurrent
	if max <= 0 {
		max = 1
	}
	if len(c.pending) >= max {
		return ErrNodeBusy
	}
	if len(c.pending) >= c.hub.cfg.MaxInFlightPerConn {
		return ErrNodeUnavailable
	}
	c.pending[reqID] = &pendingReq{
		ch:   make(chan contracts.ChatChunk, 128),
		done: make(chan struct{}),
	}
	return nil
}

func (c *conn) pendingChan(reqID string) chan contracts.ChatChunk {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pending[reqID].ch
}

func (c *conn) pendingDone(reqID string) <-chan struct{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if p, ok := c.pending[reqID]; ok {
		return p.done
	}
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (c *conn) deliverChunk(reqID string, chunk contracts.ChatChunk) {
	c.mu.RLock()
	p, ok := c.pending[reqID]
	c.mu.RUnlock()
	if !ok {
		return
	}
	select {
	case p.ch <- chunk:
	default:
		// Consumer gone slow; drop protects the read loop. The request
		// will still terminate on infer_end.
	}
}

// completePending closes out a request, delivering a final error chunk if
// provided. Idempotent. Wakes the scheduler — a slot just freed.
func (c *conn) completePending(reqID string, err error) {
	c.mu.Lock()
	p, ok := c.pending[reqID]
	if ok {
		delete(c.pending, reqID)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	p.once.Do(func() {
		if err != nil {
			p.ch <- contracts.ChatChunk{Done: true, Error: err.Error()}
		}
		close(p.done)
		close(p.ch)
	})
	c.hub.notify()
}

// failAllPending terminates every in-flight request (connection loss).
func (c *conn) failAllPending(msg string) {
	c.mu.Lock()
	reqs := make([]*pendingReq, 0, len(c.pending))
	for id, p := range c.pending {
		delete(c.pending, id)
		reqs = append(reqs, p)
	}
	c.mu.Unlock()
	for _, p := range reqs {
		p.once.Do(func() {
			p.ch <- contracts.ChatChunk{Done: true, Error: msg}
			close(p.done)
			close(p.ch)
		})
	}
}

func (c *conn) beat() {
	c.mu.Lock()
	c.lastBeatT = time.Now()
	c.mu.Unlock()
}

func (c *conn) lastBeat() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastBeatT
}

func (c *conn) setLatency(ms float64) {
	c.mu.Lock()
	c.latency = ms
	c.mu.Unlock()
}

func (c *conn) latencyMS() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latency
}

func (c *conn) inFlight() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.pending)
}

func (c *conn) close(code websocket.StatusCode, reason string) {
	c.ws.Close(code, reason)
}

// pingLoop keeps the ws connection alive and measures RTT. Runs until ctx
// is done or the ping fails.
func (c *conn) pingLoop(ctx context.Context, interval time.Duration, done <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			start := time.Now()
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				return // read loop will observe the dead connection
			}
			c.setLatency(float64(time.Since(start).Microseconds()) / 1000.0)
		}
	}
}
