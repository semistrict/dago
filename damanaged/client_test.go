package damanaged

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestListAgentsPaginatesWithAuthenticationAndBounds(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if request.URL.Path != agentsPath || request.Header.Get("X-Api-Key") != "secret" || request.URL.Query().Get("name") != "alpha" {
			t.Fatalf("request = %s headers=%v", request.URL, request.Header)
		}
		body := `{"items":[{"id":"a1","name":"alpha"}],"next_cursor":"next"}`
		if call == 2 {
			if request.URL.Query().Get("cursor") != "next" {
				t.Fatalf("cursor = %q", request.URL.Query().Get("cursor"))
			}
			body = `{"items":[{"id":"a2","name":"alpha"}],"next_cursor":null}`
		}
		return response(http.StatusOK, body), nil
	})
	client := New(&http.Client{Transport: transport}, "https://api.example.test", "secret", Options{})
	agents, err := client.ListAgents(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[0]["id"] != "a1" || agents[1]["id"] != "a2" {
		t.Fatalf("agents = %#v", agents)
	}
}

func TestGetAndDeleteAgent(t *testing.T) {
	var methods []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		methods = append(methods, request.Method)
		if request.URL.Path != agentsPath+"/agent-1" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if request.Method == http.MethodGet {
			if request.URL.Query().Get("include_files") != "true" {
				t.Fatalf("query = %v", request.URL.Query())
			}
			return response(http.StatusOK, `{"id":"agent-1","revision":"r1"}`), nil
		}
		return response(http.StatusNoContent, ""), nil
	})
	client := New(&http.Client{Transport: transport}, "https://api.example.test", "secret", Options{})
	agent, err := client.GetAgent(context.Background(), "agent-1", true)
	if err != nil || agent["revision"] != "r1" {
		t.Fatalf("agent=%#v err=%v", agent, err)
	}
	if err := client.DeleteAgent(context.Background(), "agent-1"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "GET,DELETE" {
		t.Fatalf("methods = %v", methods)
	}
}

func TestCreatePatchAndDirectoryCommitUsePinnedPayloads(t *testing.T) {
	var calls []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.Method != http.MethodGet && (request.Header.Get("Content-Type") != "application/json" || len(body) == 0) {
			t.Fatalf("headers=%v body=%q", request.Header, body)
		}
		switch request.URL.Path {
		case agentsPath:
			if request.Header.Get("Idempotency-Key") != "0123456789abcdef" {
				t.Fatalf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
			}
			return response(http.StatusCreated, `{"id":"a1","name":"agent"}`), nil
		case agentsPath + "/a1":
			return response(http.StatusOK, `{"id":"a1","name":"agent"}`), nil
		case hubPath + "/repos/-/a1/directories":
			return response(http.StatusOK, `{"commit_hash":"c1","files":{}}`), nil
		case hubPath + "/repos/-/a1/directories/commits":
			if !strings.Contains(string(body), `"parent_commit":"c1"`) || !strings.Contains(string(body), `"AGENTS.md"`) {
				t.Fatalf("commit body = %s", body)
			}
			return response(http.StatusCreated, `{"commit_hash":"c2"}`), nil
		default:
			t.Fatalf("path = %s", request.URL.Path)
			return nil, nil
		}
	})
	client := New(&http.Client{Transport: transport}, "https://api.example.test", "secret", Options{})
	if _, err := client.CreateAgent(context.Background(), map[string]any{"name": "agent"}, "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PatchAgent(context.Background(), "a1", map[string]any{"name": "agent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetAgentDirectory(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CommitAgentDirectory(context.Background(), "a1", map[string]*DirectoryFile{
		"AGENTS.md": {Type: "file", Content: "instructions"},
	}, "c1"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestClientRejectsRedirectsRetriesServerErrorsAndBoundsResponses(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return response(http.StatusServiceUnavailable, `{"detail":"later"}`), nil
		}
		return response(http.StatusOK, `{"items":[],"next_cursor":null}`), nil
	})
	client := New(&http.Client{Transport: transport}, "https://api.example.test", "secret", Options{RetryDelay: time.Nanosecond})
	if _, err := client.ListAgents(context.Background(), ""); err != nil || calls.Load() != 2 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}

	large := New(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return response(http.StatusOK, strings.Repeat("x", 65)), nil
	})}, "https://api.example.test", "secret", Options{MaxResponseBytes: 64})
	if _, err := large.ListAgents(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("error = %v", err)
	}

	redirect := New(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		result := response(http.StatusFound, "redirect refused")
		result.Header.Set("Location", "https://attacker.example/steal")
		return result, nil
	})}, "https://api.example.test", "secret", Options{RetryDelay: time.Nanosecond})
	if _, err := redirect.ListAgents(context.Background(), ""); err == nil {
		t.Fatal("expected redirect rejection")
	}
}

func TestClientRetriesOnlyIdempotentMutationsAfterServerErrors(t *testing.T) {
	var calls atomic.Int32
	client := New(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return response(http.StatusServiceUnavailable, `{"detail":"uncertain mutation result"}`), nil
		}
		if request.Header.Get("Idempotency-Key") != "0123456789abcdef" {
			t.Fatalf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
		}
		return response(http.StatusOK, `{"id":"a1"}`), nil
	})}, "https://api.example.test", "secret", Options{RetryDelay: time.Nanosecond})
	if agent, err := client.CreateAgent(context.Background(), map[string]any{"name": "agent"}, "0123456789abcdef"); err != nil || agent["id"] != "a1" {
		t.Fatalf("agent=%#v err=%v", agent, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("create calls = %d, want 2", calls.Load())
	}

	calls.Store(0)
	patchClient := New(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusServiceUnavailable, `{"detail":"uncertain mutation result"}`), nil
	})}, "https://api.example.test", "secret", Options{RetryDelay: time.Nanosecond})
	if _, err := patchClient.PatchAgent(context.Background(), "a1", map[string]any{"name": "agent"}); err == nil {
		t.Fatal("expected patch failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("patch calls = %d, want 1", calls.Load())
	}
}

func TestClientRetriesIdempotentCreateAfterTransportFailure(t *testing.T) {
	var calls atomic.Int32
	client := New(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("connection reset")
		}
		if request.Header.Get("Idempotency-Key") != "0123456789abcdef" {
			t.Fatalf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
		}
		return response(http.StatusOK, `{"id":"a1"}`), nil
	})}, "https://api.example.test", "secret", Options{RetryDelay: time.Nanosecond})
	agent, err := client.CreateAgent(context.Background(), map[string]any{"name": "agent"}, "0123456789abcdef")
	if err != nil || agent["id"] != "a1" || calls.Load() != 2 {
		t.Fatalf("agent=%#v calls=%d err=%v", agent, calls.Load(), err)
	}
}

func TestClientErrorsAreBoundedAndCancellationWins(t *testing.T) {
	client := New(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return response(http.StatusBadRequest, `{"code":"bad","detail":"invalid\ninput"}`), nil
	})}, "https://api.example.test", "secret", Options{})
	_, err := client.GetAgent(context.Background(), "a", false)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "bad" || strings.Contains(apiErr.Error(), "\n") {
		t.Fatalf("error = %#v", err)
	}

	cancelClient := New(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}, "https://api.example.test", "secret", Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cancelClient.ListAgents(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewRejectsInvalidStaticConfiguration(t *testing.T) {
	for _, invoke := range []func(){
		func() { New(nil, "https://api.example.test", "secret", Options{}) },
		func() { New(http.DefaultClient, "http://api.example.test", "secret", Options{}) },
		func() { New(http.DefaultClient, "https://user@api.example.test", "secret", Options{}) },
		func() { New(http.DefaultClient, "https://api.example.test", "", Options{}) },
		func() { New(http.DefaultClient, "https://api.example.test", "secret", Options{MaxPages: -1}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			invoke()
		}()
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
