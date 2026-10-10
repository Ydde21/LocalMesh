// Package wsjson is a thin typed wrapper over a WebSocket connection for
// the LocalMesh node<->server protocol. It serializes access and carries
// contracts.Envelope frames as JSON text messages.
package wsjson

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"

	"github.com/coder/websocket"

	"github.com/localmesh/localmesh/internal/contracts"
)

// Conn wraps a websocket.Conn with envelope-level read/write. A single
// Conn may be written to from multiple goroutines (writes are
// serialized); reads should happen from one goroutine.
type Conn struct {
	ws *websocket.Conn
}

func Wrap(ws *websocket.Conn) *Conn { return &Conn{ws: ws} }

func (c *Conn) Write(ctx context.Context, env contracts.Envelope) error {
	return c.ws.Write(ctx, websocket.MessageText, envBytes(env))
}

func (c *Conn) Read(ctx context.Context) (contracts.Envelope, error) {
	var env contracts.Envelope
	err := wsReadJSON(ctx, c.ws, &env)
	return env, err
}

func (c *Conn) Close(code websocket.StatusCode, reason string) error {
	return c.ws.Close(code, reason)
}

// CloseNow drops the connection without the close handshake. Use it
// when the peer is already known dead — a graceful Close would wait up
// to 5s for a close echo that can never arrive, stalling teardown.
func (c *Conn) CloseNow() error {
	return c.ws.CloseNow()
}

// Dial opens a client connection. If tlsCfg is non-nil it is used for
// wss:// endpoints (CA pinning lives in tlsCfg.RootCAs).
func Dial(ctx context.Context, rawurl string, header http.Header, tlsCfg *tls.Config) (*Conn, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	opts := &websocket.DialOptions{HTTPHeader: header}
	if tlsCfg != nil && (u.Scheme == "wss" || u.Scheme == "https") {
		opts.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	}
	ws, _, err := websocket.Dial(ctx, u.String(), opts)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", u.Redacted(), err)
	}
	return &Conn{ws: ws}, nil
}

// Accept upgrades an inbound HTTP request to a websocket connection.
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin check is performed by the caller where relevant; node
		// agents authenticate via bearer token and do not send Origin.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return nil, err
	}
	return &Conn{ws: ws}, nil
}

// Ping sends a protocol-level ping and waits for the pong.
func (c *Conn) Ping(ctx context.Context) error {
	return c.ws.Ping(ctx)
}

// SetReadLimit bounds inbound frame size.
func (c *Conn) SetReadLimit(n int64) { c.ws.SetReadLimit(n) }
