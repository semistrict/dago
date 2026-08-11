package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestRequireHeaderMiddleware_BlocksAPIWithoutHeader(t *testing.T) {
	t.Parallel()
	handler := RequireHeaderMiddleware("X-User-ID")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/conversations", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected status 403 for API request without required header, got %d", w.Code)
	}
}

func TestRequireHeaderMiddleware_AllowsAPIWithHeader(t *testing.T) {
	t.Parallel()
	handler := RequireHeaderMiddleware("X-User-ID")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/conversations", nil)
	req.Header.Set("X-User-ID", "user123")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 for API request with required header, got %d", w.Code)
	}
}

func TestRequireHeaderMiddleware_AllowsPublicUIWithoutHeader(t *testing.T) {
	t.Parallel()
	handler := RequireHeaderMiddleware("X-User-ID")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/", "/new", "/c/example", "/main.js", "/styles.css", "/icon.png", "/app.wasm", "/manifest.json"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected status 200 for public UI path %q without required header, got %d", path, w.Code)
		}
	}
}

func TestRequireHeaderMiddleware_BlocksPrivilegedNonAPIRoutes(t *testing.T) {
	t.Parallel()
	handler := RequireHeaderMiddleware("X-User-ID")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/version", "/settings", "/feature-flags", "/debug/pprof/", "/exit"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("expected status 403 for privileged path %q without required header, got %d", path, w.Code)
		}
	}
}

func TestRequireHeaderMiddleware_BlocksNonGETStaticLookalike(t *testing.T) {
	t.Parallel()
	handler := RequireHeaderMiddleware("X-User-ID")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/main.js", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestRegisterRoutesOnlyExposesDebugEndpointsWhenEnabled(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)

	withoutDebug := http.NewServeMux()
	(&Server{}).RegisterRoutes(withoutDebug)
	_, pattern := withoutDebug.Handler(request)
	if pattern != "/" {
		t.Fatalf("debug-disabled pattern = %q, want static fallback", pattern)
	}

	withDebug := http.NewServeMux()
	(&Server{Debug: true}).RegisterRoutes(withDebug)
	_, pattern = withDebug.Handler(request)
	if pattern != "GET /debug/pprof/" {
		t.Fatalf("debug-enabled pattern = %q", pattern)
	}
}

func TestVersionCheckReportsOnlyLocalBuildInformation(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/version-check", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, key := range []string{"current_version", "checked_at"} {
		if _, ok := body[key]; !ok {
			t.Errorf("response is missing %q: %v", key, body)
		}
	}
	for _, key := range []string{"has_update", "latest_version", "download_url", "headless_shell_update"} {
		if _, ok := body[key]; ok {
			t.Errorf("response unexpectedly includes removed release field %q", key)
		}
	}
}

func TestCompressionHandler_CompressesResponse(t *testing.T) {
	t.Parallel()
	handler := compressionHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"message": "hello world"}`))
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("expected Content-Encoding: gzip, got %q", w.Header().Get("Content-Encoding"))
	}

	// Verify we can decompress the response
	gr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	defer gr.Close()

	body, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("failed to read gzip body: %v", err)
	}

	if !bytes.Contains(body, []byte("hello world")) {
		t.Errorf("decompressed body doesn't contain expected content: %s", body)
	}
}

func TestCompressionHandler_PrefersZstd(t *testing.T) {
	t.Parallel()
	handler := compressionHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message": "hello world"}`))
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Content-Encoding"); got != "zstd" {
		t.Fatalf("Content-Encoding: got %q want zstd", got)
	}
	if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary: got %q want Accept-Encoding", got)
	}

	zr, err := zstd.NewReader(bytes.NewReader(w.Body.Bytes()), zstd.WithDecoderConcurrency(1))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()

	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read zstd body: %v", err)
	}
	if !bytes.Contains(body, []byte("hello world")) {
		t.Errorf("decompressed body doesn't contain expected content: %s", body)
	}
}

func TestRoutesPreferZstd(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	tests := []struct {
		path     string
		validate func([]byte) bool
	}{
		{"/api/models", json.Valid},
		{"/c/compressed-page", func(body []byte) bool {
			return bytes.Contains(body, []byte(`id="shelley-init"`))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
			w := httptest.NewRecorder()

			mux.ServeHTTP(w, req)

			if got := w.Header().Get("Content-Encoding"); got != "zstd" {
				t.Fatalf("Content-Encoding: got %q want zstd", got)
			}
			zr, err := zstd.NewReader(bytes.NewReader(w.Body.Bytes()), zstd.WithDecoderConcurrency(1))
			if err != nil {
				t.Fatalf("zstd reader: %v", err)
			}
			defer zr.Close()
			body, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("read zstd body: %v", err)
			}
			if !tc.validate(body) {
				t.Fatalf("unexpected decoded response: %.100s", body)
			}
		})
	}
}

func TestCompressionHandler_SkipsWhenNoAcceptEncoding(t *testing.T) {
	t.Parallel()
	handler := compressionHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"message": "hello"}`))
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	// No Accept-Encoding header
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") != "" {
		t.Errorf("expected no Content-Encoding, got %q", w.Header().Get("Content-Encoding"))
	}

	if !bytes.Contains(w.Body.Bytes(), []byte("hello")) {
		t.Errorf("body doesn't contain expected content: %s", w.Body.String())
	}
}
