//go:build !js

package openai

import (
	"net/http"

	"github.com/coder/websocket"
)

func websocketDialOptions(client *http.Client, header http.Header) *websocket.DialOptions {
	return &websocket.DialOptions{
		HTTPClient:      client,
		HTTPHeader:      header,
		CompressionMode: websocket.CompressionContextTakeover,
	}
}
