package xtermjs

import (
	"bytes"
	"embed"
	"encoding/base64"
	"io"
	"net/http"
	"time"
)

//go:embed index.html dist/app.css.br.b64 dist/app.js.br.b64
var assets embed.FS

// Handler serves the self-contained browser terminal client.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /assets/app.js", func(response http.ResponseWriter, request *http.Request) {
		serveBrotliAsset(response, request, "dist/app.js.br.b64", "app.js", "application/javascript; charset=utf-8")
	})
	mux.HandleFunc("GET /assets/app.css", func(response http.ResponseWriter, request *http.Request) {
		serveBrotliAsset(response, request, "dist/app.css.br.b64", "app.css", "text/css; charset=utf-8")
	})
	mux.HandleFunc("GET /", func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(response, request)
			return
		}
		page, readErr := assets.ReadFile("index.html")
		if readErr != nil {
			http.Error(response, "embedded terminal page is unavailable", http.StatusInternalServerError)
			return
		}
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(response, request, "index.html", time.Time{}, bytes.NewReader(page))
	})
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; img-src 'self' data:; style-src 'self' 'unsafe-inline'")
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(response, request)
	})
}

func serveBrotliAsset(response http.ResponseWriter, request *http.Request, path, name, contentType string) {
	encoded, err := assets.ReadFile(path)
	if err != nil {
		http.Error(response, "embedded browser asset is unavailable", http.StatusInternalServerError)
		return
	}
	content, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(encoded)))
	if err != nil {
		http.Error(response, "embedded browser asset is invalid", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Encoding", "br")
	response.Header().Set("Content-Type", contentType)
	http.ServeContent(response, request, name, time.Time{}, bytes.NewReader(content))
}
