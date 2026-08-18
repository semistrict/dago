package dacode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/datalon/tracing"
)

type traceRoundTripperFunc func(*http.Request) (*http.Response, error)

func (function traceRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func traceHTTPResponse(status int, body string, headers http.Header) *http.Response {
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}

func TestLangSmithProjectLookupUsesExactBoundedAuthenticatedRequest(t *testing.T) {
	secret := "private-langsmith-key"
	client := &http.Client{Transport: traceRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v1/sessions" || request.URL.Query().Get("name") != "project name" || request.URL.Query().Get("limit") != "2" || request.Header.Get("x-api-key") != secret {
			t.Errorf("request = %s?%s headers=%v", request.URL.Path, request.URL.RawQuery, request.Header)
		}
		return traceHTTPResponse(http.StatusOK, `[{"id":"project-id","tenant_id":"tenant-id","name":"project name"}]`, nil), nil
	})}
	lookup := newLangSmithProjectLookup(client, "https://api.example", "https://smith.example", secret, langSmithProjectLookupOptions{})
	link, err := lookup.ProjectURL(t.Context(), "project name")
	if err != nil || link != "https://smith.example/o/tenant-id/projects/p/project-id" {
		t.Fatalf("project URL = %q, %v", link, err)
	}
	for _, rendered := range []string{fmt.Sprint(lookup), fmt.Sprintf("%#v", lookup)} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("lookup formatting leaked credential: %q", rendered)
		}
	}
}

func TestLangSmithProjectLookupCancellationRedirectsAndErrorsAreSecretSafe(t *testing.T) {
	secret := "provider-secret"
	tests := []struct {
		name      string
		transport traceRoundTripperFunc
	}{
		{name: "provider", transport: func(*http.Request) (*http.Response, error) {
			return traceHTTPResponse(http.StatusUnauthorized, secret, nil), nil
		}},
		{name: "redirect", transport: func(*http.Request) (*http.Response, error) {
			return traceHTTPResponse(http.StatusFound, "", http.Header{"Location": []string{"https://other.example/steal"}}), nil
		}},
		{name: "oversized", transport: func(*http.Request) (*http.Response, error) {
			return traceHTTPResponse(http.StatusOK, strings.Repeat("x", defaultLangSmithProjectResponseBytes+1), nil), nil
		}},
		{name: "duplicate", transport: func(*http.Request) (*http.Response, error) {
			return traceHTTPResponse(http.StatusOK, `[{"id":"one","tenant_id":"tenant","name":"project"},{"id":"two","tenant_id":"tenant","name":"project"}]`, nil), nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookup := newLangSmithProjectLookup(&http.Client{Transport: test.transport}, "https://api.example", "https://smith.example", "key", langSmithProjectLookupOptions{})
			_, err := lookup.ProjectURL(t.Context(), "project")
			if !errors.Is(err, tracing.ErrProjectLookup) || strings.Contains(err.Error(), secret) {
				t.Fatalf("lookup error = %v", err)
			}
		})
	}

	started := make(chan struct{})
	client := &http.Client{Transport: traceRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	lookup := newLangSmithProjectLookup(client, "https://api.example", "https://smith.example", "key", langSmithProjectLookupOptions{Timeout: 30 * time.Millisecond})
	_, err := lookup.ProjectURL(t.Context(), "project")
	<-started
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestLangSmithProjectLookupRejectsUnsafeStaticAndDynamicInputs(t *testing.T) {
	client := &http.Client{}
	for _, configure := range []func(){
		func() {
			newLangSmithProjectLookup(client, "http://api.example", "https://smith.example", "key", langSmithProjectLookupOptions{})
		},
		func() {
			newLangSmithProjectLookup(client, "https://api.example", "https://user@smith.example", "key", langSmithProjectLookupOptions{})
		},
		func() {
			newLangSmithProjectLookup(client, "https://api.example", "https://smith.example/path", "key", langSmithProjectLookupOptions{})
		},
		func() {
			newLangSmithProjectLookup(client, "https://api.example", "https://smith.example", "bad\nkey", langSmithProjectLookupOptions{})
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("unsafe static input did not panic")
				}
			}()
			configure()
		}()
	}
	transport := traceRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return traceHTTPResponse(http.StatusOK, `[{"id":"../escape","tenant_id":"tenant","name":"project"}]`, nil), nil
	})
	lookup := newLangSmithProjectLookup(&http.Client{Transport: transport}, "https://api.example", "https://smith.example", "key", langSmithProjectLookupOptions{})
	if _, err := lookup.ProjectURL(t.Context(), " project "); !errors.Is(err, tracing.ErrProjectLookup) {
		t.Fatalf("unsafe project name = %v", err)
	}
	if _, err := lookup.ProjectURL(t.Context(), "project"); !errors.Is(err, tracing.ErrProjectLookup) {
		t.Fatalf("unsafe project metadata = %v", err)
	}
}
