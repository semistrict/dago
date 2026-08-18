package daupdate

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestHTTPSourceUsesFixedHTTPSOriginWithoutRedirects(t *testing.T) {
	var paths []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.String())
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 2, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	})}
	source := NewHTTPSource(client, "https://releases.example/channels")
	manifest, err := source.FetchManifest(context.Background(), "stable", 100)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Close()
	artifact, err := source.FetchArtifact(context.Background(), "https://releases.example/artifacts/dacode", 100)
	if err != nil {
		t.Fatal(err)
	}
	artifact.Close()
	if len(paths) != 2 || paths[0] != "https://releases.example/channels/stable.json" || paths[1] != "https://releases.example/artifacts/dacode" {
		t.Fatalf("paths = %#v", paths)
	}
	if _, err := source.FetchArtifact(context.Background(), "https://other.example/dacode", 100); err == nil {
		t.Fatal("cross-origin artifact accepted")
	}
}

func TestHTTPSourceRejectsInvalidConfigurationAndOversizedResponses(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 101, Body: io.NopCloser(strings.NewReader("x")), Header: http.Header{}}, nil
	})}
	source := NewHTTPSource(client, "https://releases.example/")
	if _, err := source.FetchManifest(context.Background(), "stable", 100); err == nil {
		t.Fatal("oversized response accepted")
	}
	for _, value := range []string{"http://releases.example", "https://user@example.com", "https://releases.example/?query=value"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewHTTPSource(%q) did not panic", value)
				}
			}()
			NewHTTPSource(client, value)
		}()
	}
}
