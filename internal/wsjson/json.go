package wsjson

import (
	"context"
	"encoding/json"

	"github.com/coder/websocket"

	"github.com/localmesh/localmesh/internal/contracts"
)

func envBytes(env contracts.Envelope) []byte {
	b, _ := json.Marshal(env)
	return b
}

func wsReadJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	_, data, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
