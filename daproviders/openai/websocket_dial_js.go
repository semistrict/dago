//go:build js

package openai

import (
	"net/http"

	"github.com/coder/websocket"
)

func websocketDialOptions(*http.Client, http.Header) *websocket.DialOptions {
	// Browser WebSockets do not expose request headers or compression controls.
	return nil
}
