package ollama

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestDiscoverUsesLiteralLoopbackAndReturnsDeterministicNames(t *testing.T) {
	calls := 0
	discovery := NewDiscovery(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.String() != "http://127.0.0.1:11434/api/tags" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Accept") != "application/json" || request.Header.Get("Authorization") != "" {
			t.Fatalf("headers = %#v", request.Header)
		}
		deadline, ok := request.Context().Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
			t.Fatalf("request deadline = %v, %t", deadline, ok)
		}
		return response(http.StatusOK, `{"models":[{"name":"qwen3:4b"},{"name":"llama3"},{"name":"qwen3:4b"}]}`), nil
	}), "http://localhost:11434/", DiscoveryOptions{})
	if calls != 0 || discovery.Endpoint() != DefaultEndpoint {
		t.Fatalf("constructor performed I/O or endpoint = %q, calls = %d", discovery.Endpoint(), calls)
	}
	models, err := discovery.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(models, ",") != "llama3,qwen3:4b" || calls != 1 {
		t.Fatalf("models = %#v, calls = %d", models, calls)
	}
}

func TestDiscoveryRejectsNonLocalAndMalformedStaticInputs(t *testing.T) {
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid construction reached transport")
		return nil, nil
	})
	for _, endpoint := range []string{
		"https://example.com", "http://0.0.0.0:11434", "ftp://127.0.0.1:11434",
		"http://user@127.0.0.1:11434", "http://127.0.0.1:11434/api", "http://127.0.0.1:11434?key=value",
	} {
		t.Run(endpoint, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid endpoint did not panic")
				}
			}()
			NewDiscovery(transport, endpoint, DiscoveryOptions{})
		})
	}
	var typedNil *http.Transport
	for _, value := range []http.RoundTripper{nil, typedNil} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("nil transport did not panic")
				}
			}()
			NewDiscovery(value, "", DiscoveryOptions{})
		}()
	}
	for _, options := range []DiscoveryOptions{
		{Timeout: -time.Second}, {Timeout: 31 * time.Second}, {MaxResponseBytes: -1},
		{MaxModels: 4097}, {MaxNameBytes: 4097},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("invalid options were accepted: %#v", options)
				}
			}()
			NewDiscovery(transport, "", options)
		}()
	}
}

func TestDiscoveryBoundsAndRejectsAdversarialResponses(t *testing.T) {
	for name, test := range map[string]struct {
		body    string
		options DiscoveryOptions
		want    error
	}{
		"malformed":     {body: `{`, want: ErrInvalidResponse},
		"missing array": {body: `{}`, want: ErrInvalidResponse},
		"wrong shape":   {body: `{"models":"llama3"}`, want: ErrInvalidResponse},
		"unsafe name":   {body: "{\"models\":[{\"name\":\"safe\\nforged\"}]}", want: ErrInvalidResponse},
		"padded name":   {body: `{"models":[{"name":" llama3"}]}`, want: ErrInvalidResponse},
		"name bound":    {body: `{"models":[{"name":"llama3"}]}`, options: DiscoveryOptions{MaxNameBytes: 5}, want: ErrInvalidResponse},
		"model bound":   {body: `{"models":[{"name":"one"},{"name":"two"}]}`, options: DiscoveryOptions{MaxModels: 1}, want: ErrLimit},
		"byte bound":    {body: `{"models":[]}`, options: DiscoveryOptions{MaxResponseBytes: 4}, want: ErrLimit},
	} {
		t.Run(name, func(t *testing.T) {
			discovery := NewDiscovery(roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, test.body), nil
			}), "", test.options)
			if _, err := discovery.Discover(t.Context()); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestDiscoveryPropagatesCancellationAndContainsTransportFailures(t *testing.T) {
	started := make(chan struct{})
	discovery := NewDiscovery(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	}), "", DiscoveryOptions{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := discovery.Discover(ctx)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}

	for name, transport := range map[string]http.RoundTripper{
		"error":        roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("credential-value") }),
		"panic":        roundTripperFunc(func(*http.Request) (*http.Response, error) { panic("credential-value") }),
		"nil response": roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil }),
		"status": roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return response(http.StatusUnauthorized, "credential-value"), nil
		}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewDiscovery(transport, "", DiscoveryOptions{}).Discover(t.Context())
			if err == nil || strings.Contains(err.Error(), "credential-value") {
				t.Fatalf("failure was missing or leaked transport detail: %v", err)
			}
		})
	}
}

func TestDiscoveryIsSafeForConcurrentExplicitRefreshes(t *testing.T) {
	var lock sync.Mutex
	calls := 0
	discovery := NewDiscovery(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		lock.Lock()
		calls++
		lock.Unlock()
		return response(http.StatusOK, `{"models":[{"name":"llama3"}]}`), nil
	}), "http://[::1]:11434", DiscoveryOptions{})
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			models, err := discovery.Discover(t.Context())
			if err != nil || len(models) != 1 || models[0] != "llama3" {
				t.Errorf("models = %#v, err = %v", models, err)
			}
		}()
	}
	wait.Wait()
	lock.Lock()
	defer lock.Unlock()
	if calls != 16 {
		t.Fatalf("calls = %d", calls)
	}
}
