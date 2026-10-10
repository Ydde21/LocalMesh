package wsjson

// conn_test.go exercises the envelope transport over a loopback
// httptest websocket pair — concurrent writers, close semantics, and
// the read-limit bound. In-process only.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/localmesh/localmesh/internal/contracts"
)

// wsPair dials one client/server Conn pair. The server conn is released
// to the test through a channel; the httptest handler returns after
// Accept, leaving the hijacked socket open.
func wsPair(t *testing.T) (client, server *Conn) {
	t.Helper()
	ch := make(chan *Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		ch <- c
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + srv.URL[len("http"):]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, wsURL, nil, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	client = c
	select {
	case server = <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("server conn never arrived")
	}
	t.Cleanup(func() {
		// Close's handshake only completes while the peer is reading —
		// drain both sides so the echoes land promptly.
		go drain(client)
		go drain(server)
		client.Close(websocket.StatusNormalClosure, "test done")
		server.Close(websocket.StatusNormalClosure, "test done")
	})
	return client, server
}

// drain reads a conn until it dies — keeps control frames (ping, pong,
// close) processed for tests that only exercise the write/read path.
func drain(c *Conn) {
	for {
		if _, err := c.Read(context.Background()); err != nil {
			return
		}
	}
}

// TestEnvelopeRoundTrip sends envelopes in both directions and verifies
// payload fidelity end to end.
func TestEnvelopeRoundTrip(t *testing.T) {
	client, server := wsPair(t)
	ctx := context.Background()

	sentAt := time.Unix(1700000000, 0).UTC()
	in := contracts.Envelope{
		ID:      "abc",
		Type:    contracts.MsgPing,
		Payload: contracts.MarshalPayload(contracts.Ping{SentAt: sentAt}),
	}
	if err := client.Write(ctx, in); err != nil {
		t.Fatalf("client write: %v", err)
	}
	got, err := server.Read(ctx)
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	if got.ID != "abc" || got.Type != contracts.MsgPing {
		t.Fatalf("envelope mangled: %+v", got)
	}
	var p contracts.Ping
	if err := got.Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !p.SentAt.Equal(sentAt) {
		t.Fatalf("payload round-trip lost data: %v != %v", p.SentAt, sentAt)
	}

	back := contracts.Envelope{ID: "r1", Type: contracts.MsgPong, ReplyTo: got.ID}
	if err := server.Write(ctx, back); err != nil {
		t.Fatalf("server write: %v", err)
	}
	got2, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if got2.ReplyTo != "abc" || got2.Type != contracts.MsgPong {
		t.Fatalf("reply envelope mangled: %+v", got2)
	}
}

// TestConcurrentWritesSerialized: wsjson.Conn is documented as safe for
// concurrent writers — every frame must arrive whole and decodable, no
// interleaved bytes.
func TestConcurrentWritesSerialized(t *testing.T) {
	client, server := wsPair(t)
	const writers = 8
	const perWriter = 25

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for i := 0; i < perWriter; i++ {
				e := contracts.Envelope{
					ID:      fmt.Sprintf("w%d-%d", w, i),
					Type:    contracts.MsgHeartbeat,
					Payload: contracts.MarshalPayload(contracts.Heartbeat{}),
				}
				_ = client.Write(ctx, e)
			}
		}(w)
	}

	read := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		seen := map[string]bool{}
		for i := 0; i < writers*perWriter; i++ {
			env, err := server.Read(ctx)
			if err != nil {
				read <- err
				return
			}
			if env.Type != contracts.MsgHeartbeat || env.ID == "" {
				read <- errors.New("corrupt envelope: bad type/id")
				return
			}
			if seen[env.ID] {
				read <- errors.New("corrupt envelope: duplicate id " + env.ID)
				return
			}
			seen[env.ID] = true
		}
		read <- nil
	}()

	wg.Wait()
	if err := <-read; err != nil {
		t.Fatalf("concurrent writes corrupted the stream: %v", err)
	}
}

// TestReadLimitEnforced: a peer sending a frame over the configured
// limit gets the connection torn down rather than an unbounded read.
func TestReadLimitEnforced(t *testing.T) {
	client, server := wsPair(t)
	server.SetReadLimit(512)

	big := contracts.Envelope{
		ID:   "big",
		Type: contracts.MsgHeartbeat,
		Payload: contracts.MarshalPayload(contracts.Heartbeat{
			Metrics: contracts.Metrics{LoadedModels: []string{strings.Repeat("x", 2048)}},
		}),
	}
	if err := client.Write(context.Background(), big); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := server.Read(context.Background()); err == nil {
		t.Fatal("oversize frame read without error")
	}
}

// TestCloseSemantics: after Close the peer observes the close status,
// further reads fail, writes fail, and double Close is safe. The close
// handshake needs the peer reading, so server.Read is what completes it.
func TestCloseSemantics(t *testing.T) {
	client, server := wsPair(t)

	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close(websocket.StatusNormalClosure, "bye") }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := server.Read(ctx)
	if err == nil {
		t.Fatal("read on closed conn should fail")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusNormalClosure {
		t.Fatalf("close status = %v, want normal closure (err: %v)", status, err)
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close handshake: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close never completed — peer echo missing")
	}

	// Double close must not panic.
	client.Close(websocket.StatusNormalClosure, "again")
	// Writes on a closed conn fail, they do not panic.
	dead := contracts.Envelope{ID: "x", Type: contracts.MsgPong}
	if err := client.Write(context.Background(), dead); err == nil {
		t.Fatal("write on closed conn should fail")
	}
}

// TestPingPong: a protocol ping round trip. Both ends need a concurrent
// reader — the pong is only observed by the pinging side's Reader.
func TestPingPong(t *testing.T) {
	client, server := wsPair(t)

	go drain(server)
	go drain(client)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := server.Ping(ctx); err != nil {
		t.Fatalf("server Ping: %v", err)
	}
}
