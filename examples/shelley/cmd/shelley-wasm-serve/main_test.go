package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandlerServesAssetsAndClientRoutes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("browser app"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.js"), []byte("application"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := newHandler(root)

	for _, test := range []struct {
		path string
		want int
		body string
	}{
		{path: "/", want: http.StatusOK, body: "browser app"},
		{path: "/c/a-conversation", want: http.StatusOK, body: "browser app"},
		{path: "/main.js", want: http.StatusOK, body: "application"},
		{path: "/missing.js", want: http.StatusNotFound, body: "404 page not found"},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want || !containsText(response.Body.String(), test.body) {
				t.Fatalf("response = %d %q, want %d containing %q", response.Code, response.Body.String(), test.want, test.body)
			}
		})
	}
}

func TestHandlerServesApplicationBelowBasePath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("browser app"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.js"), []byte("application"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := newHandlerAt(root, "/dago/")

	for _, test := range []struct {
		path string
		want int
		body string
	}{
		{path: "/dago/", want: http.StatusOK, body: "browser app"},
		{path: "/dago/c/a-conversation", want: http.StatusOK, body: "browser app"},
		{path: "/dago/main.js", want: http.StatusOK, body: "application"},
		{path: "/main.js", want: http.StatusNotFound, body: "404 page not found"},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want || !containsText(response.Body.String(), test.body) {
				t.Fatalf("response = %d %q, want %d containing %q", response.Code, response.Body.String(), test.want, test.body)
			}
		})
	}
}

func containsText(value, substring string) bool {
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}
