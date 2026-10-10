package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/localmesh/localmesh/internal/agent/engine"
	"github.com/localmesh/localmesh/internal/agent/hardware"
	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/wsjson"
)

// Agent is a running node agent.
type Agent struct {
	cfg     *Config
	log     *slog.Logger
	engines []engine.Engine

	hw contracts.HardwareInfo // detected once at boot, refreshed on demand

	active    atomic.Int32
	cancels   sync.Map // requestID -> context.CancelFunc
	opCancels sync.Map // operationID -> context.CancelFunc
	opAborted sync.Map // operationID -> struct{} (admin-invoked aborts)
}

func New(cfg *Config, log *slog.Logger) *Agent {
	engs := []engine.Engine{
		engine.NewOllama(cfg.OllamaURL),
	}
	return &Agent{cfg: cfg, log: log.With("component", "agent"), engines: engs}
}

// errAuthRejected marks a 401/403 from the connect handshake — the node
// credential is bad or the node is not approved/revoked. Unlike network
// errors this does not fix itself, so retries are bounded.
var errAuthRejected = errors.New("node credential rejected")

// maxAuthRejects bounds consecutive auth rejections before Run exits
// nonzero — the service manager records a failed agent instead of the
// agent hammering the control plane forever.
const maxAuthRejects = 3

// Run connects to the control plane and serves until ctx is cancelled.
// Reconnects with exponential backoff (±20% jitter) on failures.
func (a *Agent) Run(ctx context.Context) error {
	a.hw = hardware.Detect(ctx)
	a.log.Info("hardware detected",
		"cpu", a.hw.CPUModel, "cores", a.hw.CPUCores,
		"mem_gb", a.hw.MemoryBytes/(1<<30), "gpus", len(a.hw.GPUs))

	backoff := time.Second
	authRejects := 0
	for {
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errAuthRejected) {
			authRejects++
			if authRejects >= maxAuthRejects {
				return fmt.Errorf("node credential rejected %d times — node revoked or deleted? re-enroll with 'localmesh-agent join'", authRejects)
			}
		} else {
			authRejects = 0
		}
		sleep := jittered(backoff)
		a.log.Warn("connection lost, reconnecting", "err", err, "in", sleep)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(sleep):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// jittered applies ±20% jitter to a backoff interval.
func jittered(d time.Duration) time.Duration {
	return d * time.Duration(80+rand.IntN(40)) / 100
}

// dialWS opens the control-plane socket. When the upgrade is rejected
// with 401/403 the error wraps errAuthRejected so Run can stop retrying;
// anything else is a transient failure worth backing off for.
func (a *Agent) dialWS(ctx context.Context) (*wsjson.Conn, error) {
	u, err := url.Parse(a.cfg.WSURL())
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+a.cfg.NodeToken)
	opts := &websocket.DialOptions{HTTPHeader: hdr}
	if tlsCfg := a.cfg.TLSConfig(); tlsCfg != nil && u.Scheme == "wss" {
		opts.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	}
	ws, resp, err := websocket.Dial(ctx, u.String(), opts)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			reason := strings.TrimSpace(readLimit(resp.Body, 256))
			return nil, fmt.Errorf("%w: HTTP %d %s", errAuthRejected, resp.StatusCode, reason)
		}
		return nil, fmt.Errorf("dial %s: %w", u.Redacted(), err)
	}
	return wsjson.Wrap(ws), nil
}

// session runs one connection lifecycle.
func (a *Agent) session(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	c, err := a.dialWS(dctx)
	cancel()
	if err != nil {
		return err
	}
	defer c.Close(websocket.StatusNormalClosure, "bye")

	// Hello handshake.
	if err := c.Write(ctx, contracts.Envelope{
		ID:   uuid.NewString(),
		Type: contracts.MsgHello,
		Payload: contracts.MarshalPayload(contracts.Hello{
			NodeID:          a.cfg.NodeID,
			ProtocolVersion: contracts.ProtocolVersion,
			AgentVersion:    a.hw.AgentVersion,
		}),
	}); err != nil {
		return err
	}
	env, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if env.Type == contracts.MsgError {
		var e contracts.ErrorPayload
		env.Decode(&e)
		return errors.New("server rejected connection: " + e.Message)
	}
	if env.Type != contracts.MsgHelloAck {
		return errors.New("expected hello_ack, got " + env.Type)
	}
	var ack contracts.HelloAck
	env.Decode(&ack)
	heartbeat := time.Duration(ack.HeartbeatSeconds) * time.Second
	if heartbeat <= 0 {
		heartbeat = 10 * time.Second
	}
	a.log.Info("connected to control plane", "server", a.cfg.ServerURL,
		"heartbeat", heartbeat)

	// Initial capability report.
	a.reportCapabilities(ctx, c)

	// Heartbeat loop.
	hbDone := make(chan struct{})
	go a.heartbeatLoop(ctx, c, heartbeat, hbDone)
	defer close(hbDone)

	// Read/dispatch loop.
	for {
		env, err := c.Read(ctx)
		if err != nil {
			return err
		}
		a.handle(ctx, c, env)
	}
}

func (a *Agent) heartbeatLoop(ctx context.Context, c *wsjson.Conn, every time.Duration, done <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	var lastEngines string
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			// Protocol-level ping with a bounded deadline: detects a
			// half-dead socket (e.g. after sleep/wake) within one
			// heartbeat interval instead of waiting on TCP timeouts.
			// Closing the conn unblocks the session read loop.
			pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				a.log.Warn("control plane ping failed, reconnecting", "err", err)
				c.Close(websocket.StatusInternalError, "ping timeout")
				return
			}
			loaded, measured := a.loadedModels(ctx)
			m := hardware.Metrics(ctx, int(a.active.Load()), loaded)
			m.LoadedMeasured = measured
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			c.Write(wctx, contracts.Envelope{
				ID:      uuid.NewString(),
				Type:    contracts.MsgHeartbeat,
				Payload: contracts.MarshalPayload(contracts.Heartbeat{Metrics: m}),
			})
			cancel()

			// Engine health changes (Ollama restart/crash) must reach the
			// scheduler quickly — Detect is one cheap /api/version call,
			// and a full capability report only ships on actual change.
			sig := a.engineSignature(ctx)
			if lastEngines != "" && sig != lastEngines {
				a.log.Info("engine state changed, re-reporting capabilities")
				a.reportCapabilities(ctx, c)
			}
			lastEngines = sig
		}
	}
}

// engineSignature summarizes each engine's health+version so the
// heartbeat can tell when the cluster-visible engine state changed.
func (a *Agent) engineSignature(ctx context.Context) string {
	var sb strings.Builder
	for _, e := range a.engines {
		ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
		info, err := e.Detect(ctx2)
		cancel()
		sb.WriteString(e.Type())
		sb.WriteByte(':')
		if err != nil || info == nil {
			sb.WriteString("down;")
			continue
		}
		sb.WriteString(info.Version)
		sb.WriteByte('=')
		sb.WriteString(strconv.FormatBool(info.Healthy))
		sb.WriteByte(';')
	}
	return sb.String()
}

// reportCapabilities pushes hardware + engine + model inventory.
func (a *Agent) reportCapabilities(ctx context.Context, c *wsjson.Conn) {
	rep := contracts.CapabilityReport{Hardware: a.hw}
	for _, e := range a.engines {
		ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, err := e.Detect(ctx2)
		cancel()
		if err != nil || info == nil {
			a.log.Debug("engine not detected", "type", e.Type(), "err", err)
			rep.Engines = append(rep.Engines, contracts.EngineInfo{
				Type: e.Type(), Healthy: false,
			})
			continue
		}
		rep.Engines = append(rep.Engines, *info)
		ctx3, cancel3 := context.WithTimeout(ctx, 15*time.Second)
		models, err := e.Models(ctx3)
		cancel3()
		if err != nil {
			a.log.Warn("list models", "engine", e.Type(), "err", err)
		}
		rep.Models = append(rep.Models, models...)
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Write(wctx, contracts.Envelope{
		ID:      uuid.NewString(),
		Type:    contracts.MsgCapability,
		Payload: contracts.MarshalPayload(rep),
	}); err != nil {
		a.log.Warn("capability report failed", "err", err)
		return
	}
	a.log.Info("capability report sent",
		"engines", len(rep.Engines), "models", len(rep.Models))
}

// loadedModels returns warm model names and whether at least one engine
// answered — callers set LoadedMeasured only when the data is real.
func (a *Agent) loadedModels(ctx context.Context) ([]string, bool) {
	var out []string
	measured := false
	for _, e := range a.engines {
		ctx2, cancel := context.WithTimeout(ctx, 8*time.Second)
		models, err := e.Models(ctx2)
		cancel()
		if err != nil {
			continue
		}
		measured = true
		for _, m := range models {
			if m.Loaded {
				out = append(out, m.Name)
			}
		}
	}
	return out, measured
}

func (a *Agent) handle(ctx context.Context, c *wsjson.Conn, env contracts.Envelope) {
	switch env.Type {
	case contracts.MsgInferReq:
		var req contracts.InferenceRequest
		if err := env.Decode(&req); err != nil {
			return
		}
		go a.runInference(ctx, c, req)

	case contracts.MsgInferCancel:
		var cc contracts.InferCancel
		if env.Decode(&cc) == nil {
			if cancel, ok := a.cancels.LoadAndDelete(cc.RequestID); ok {
				cancel.(context.CancelFunc)()
			}
		}

	case contracts.MsgModelPull:
		var p contracts.ModelPull
		if err := env.Decode(&p); err != nil {
			return
		}
		go a.runModelOp(ctx, c, p.OperationID, p.Model, "pull")

	case contracts.MsgModelDelete:
		var d contracts.ModelDelete
		if err := env.Decode(&d); err != nil {
			return
		}
		go a.runModelOp(ctx, c, d.OperationID, d.Model, "delete")

	case contracts.MsgModelOpAbort:
		var ab contracts.ModelOpAbort
		if env.Decode(&ab) != nil || ab.OperationID == "" {
			return
		}
		if cancel, ok := a.opCancels.LoadAndDelete(ab.OperationID); ok {
			a.opAborted.Store(ab.OperationID, struct{}{})
			cancel.(context.CancelFunc)()
		}

	case contracts.MsgPing:
		var p contracts.Ping
		env.Decode(&p)
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		c.Write(wctx, contracts.Envelope{
			ID:      uuid.NewString(),
			Type:    contracts.MsgPong,
			ReplyTo: env.ID,
			Payload: contracts.MarshalPayload(contracts.Pong{SentAt: p.SentAt}),
		})
		cancel()
	}
}

// runInference executes a dispatched request against the owning engine.
// Security: the request contains only model+messages; the agent executes
// nothing else from the wire.
func (a *Agent) runInference(parent context.Context, c *wsjson.Conn, req contracts.InferenceRequest) {
	a.active.Add(1)
	defer a.active.Add(-1)

	ctx, cancel := context.WithCancel(parent)
	a.cancels.Store(req.RequestID, cancel)
	defer func() {
		a.cancels.Delete(req.RequestID)
		cancel()
	}()

	send := func(env contracts.Envelope) {
		wctx, wcancel := context.WithTimeout(parent, 15*time.Second)
		defer wcancel()
		c.Write(wctx, env)
	}

	eng := a.engineFor(req.Model)
	if eng == nil {
		send(contracts.Envelope{
			ID: uuid.NewString(), Type: contracts.MsgInferError, ReplyTo: req.RequestID,
			Payload: contracts.MarshalPayload(contracts.ErrorPayload{
				Code: "no_engine", Message: "no local engine hosts model " + req.Model,
			}),
		})
		return
	}

	emit := func(chunk contracts.ChatChunk) {
		send(contracts.Envelope{
			ID: uuid.NewString(), Type: contracts.MsgInferChunk, ReplyTo: req.RequestID,
			Payload: contracts.MarshalPayload(chunk),
		})
	}
	err := eng.Chat(ctx, req, emit)
	if err != nil {
		code := "engine_error"
		if errors.Is(err, context.Canceled) {
			code = "cancelled"
		}
		send(contracts.Envelope{
			ID: uuid.NewString(), Type: contracts.MsgInferError, ReplyTo: req.RequestID,
			Payload: contracts.MarshalPayload(contracts.ErrorPayload{Code: code, Message: err.Error()}),
		})
		return
	}
	send(contracts.Envelope{
		ID: uuid.NewString(), Type: contracts.MsgInferEnd, ReplyTo: req.RequestID,
	})
}

// engineFor picks the engine hosting a model.
func (a *Agent) engineFor(model string) engine.Engine {
	for _, e := range a.engines {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		models, err := e.Models(ctx)
		cancel()
		if err != nil {
			continue
		}
		for _, m := range models {
			if m.Name == model {
				return e
			}
		}
	}
	return nil
}

func (a *Agent) runModelOp(ctx context.Context, c *wsjson.Conn, opID, model, op string) {
	opCtx, opCancel := context.WithCancel(ctx)
	a.opCancels.Store(opID, opCancel)
	defer func() {
		a.opCancels.Delete(opID)
		opCancel()
	}()

	send := func(env contracts.Envelope) {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		c.Write(wctx, env)
	}
	progress := func(status string, completed, total uint64) {
		send(contracts.Envelope{
			ID: uuid.NewString(), Type: contracts.MsgOpProgress,
			Payload: contracts.MarshalPayload(contracts.OpProgress{
				OperationID: opID, Model: model,
				Status: status, Completed: completed, Total: total,
			}),
		})
	}

	var err error
	for _, e := range a.engines {
		if e.Type() != "ollama" {
			continue
		}
		switch op {
		case "pull":
			err = e.Pull(opCtx, model, progress)
		case "delete":
			err = e.Delete(opCtx, model)
		}
		break
	}

	_, aborted := a.opAborted.LoadAndDelete(opID)
	success := err == nil
	errStr := ""
	if err != nil {
		if aborted {
			errStr = "cancelled by administrator"
		} else {
			errStr = err.Error()
		}
	}
	send(contracts.Envelope{
		ID: uuid.NewString(), Type: contracts.MsgOpDone,
		Payload: contracts.MarshalPayload(contracts.OpDone{
			OperationID: opID, Model: model, Success: success, Cancelled: aborted,
			Error: errStr,
		}),
	})
	// Refresh model inventory.
	a.reportCapabilities(ctx, c)
}
