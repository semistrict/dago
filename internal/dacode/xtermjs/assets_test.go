package xtermjs

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesPageAndBundledAssets(t *testing.T) {
	handler := Handler()
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "/", want: "dacode terminal"},
	} {
		request := httptest.NewRequest("GET", test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Errorf("GET %s status = %d", test.path, response.Code)
		}
		if !strings.Contains(response.Body.String(), test.want) {
			t.Errorf("GET %s response does not contain %q", test.path, test.want)
		}
		if policy := response.Header().Get("Content-Security-Policy"); !strings.Contains(policy, "style-src 'self' 'unsafe-inline'") {
			t.Errorf("GET %s CSP does not permit xterm.js dynamic styles: %q", test.path, policy)
		}
	}
	for _, name := range []string{"app.js", "app.css"} {
		request := httptest.NewRequest("GET", "/assets/"+name, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 || response.Header().Get("Content-Encoding") != "br" {
			t.Fatalf("%s response = %d, encoding %q", name, response.Code, response.Header().Get("Content-Encoding"))
		}
		embedded, err := assets.ReadFile("dist/" + name + ".br.b64")
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(embedded)))
		if err != nil {
			t.Fatal(err)
		}
		if response.Body.String() != string(decoded) {
			t.Fatalf("served %s does not match the embedded Brotli asset", name)
		}
	}
}
