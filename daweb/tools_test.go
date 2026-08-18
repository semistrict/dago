package daweb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/semistrict/dago/datool"
)

func TestToolsAreOptInAndTavilyIsKeyGated(t *testing.T) {
	client := NewClient(Options{Resolver: fixedResolver("8.8.8.8")})
	withoutSearch := Tools(client, "")
	if len(withoutSearch) != 2 || withoutSearch[0].Definition().Name != "http_request" || withoutSearch[1].Definition().Name != "fetch_url" {
		t.Fatalf("tools without key = %v, %v", withoutSearch[0].Definition().Name, withoutSearch[1].Definition().Name)
	}
	withSearch := Tools(client, "tvly-secret")
	if len(withSearch) != 3 || withSearch[2].Definition().Name != "web_search" {
		t.Fatalf("tools with key = %d", len(withSearch))
	}
	defer func() {
		if recover() == nil {
			t.Fatal("missing required API key should panic at static construction")
		}
	}()
	NewWebSearchTool(client, "")
}

func TestHTTPRequestToolDefaultsToGET(t *testing.T) {
	client := NewClient(Options{Resolver: fixedResolver("8.8.8.8")})
	var got *http.Request
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		got = request
		return response(request, http.StatusOK, "ok", map[string]string{"Content-Type": "text/plain"}), nil
	}
	result, err := NewHTTPRequestTool(client).Execute(context.Background(), json.RawMessage(`{"url":"https://public.example/page"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodGet || !strings.Contains(result.Content[0].Text, `"body":"ok"`) {
		t.Fatalf("request=%+v result=%+v", got, result)
	}
}

func TestFetchURLConvertsHTMLSkipsActiveContentAndTruncates(t *testing.T) {
	client := NewClient(Options{Resolver: fixedResolver("8.8.8.8"), MaxRenderedBytes: 28})
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		body := `<html><body><h1>Title</h1><script>steal()</script><style>.bad{}</style><p>Hello <strong>world</strong> and more content.</p></body></html>`
		return response(request, http.StatusOK, body, map[string]string{"Content-Type": "text/html"}), nil
	}
	result, err := NewFetchURLTool(client).Execute(context.Background(), json.RawMessage(`{"url":"https://public.example/page"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded FetchResult
	if err := json.Unmarshal(result.Structured, &decoded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decoded.MarkdownContent, "# Title") || strings.Contains(decoded.MarkdownContent, "steal") || strings.Contains(decoded.MarkdownContent, ".bad") || !decoded.Truncated {
		t.Fatalf("fetch content = %q, truncated=%v", decoded.MarkdownContent, decoded.Truncated)
	}
	if len(decoded.MarkdownContent) > 28 || decoded.ContentLength != len(decoded.MarkdownContent) {
		t.Fatalf("bounded length = %d, reported = %d", len(decoded.MarkdownContent), decoded.ContentLength)
	}
}

func TestHTMLConversionFallsBackToSafeText(t *testing.T) {
	content := htmlToMarkdownWith(`<p>Hello <strong>world</strong></p><script>steal()</script>`, func(string) (string, error) {
		return "", errors.New("conversion failed")
	})
	if !strings.Contains(content, "Hello") || !strings.Contains(content, "world") || strings.Contains(content, "steal") || strings.Contains(content, "<strong>") {
		t.Fatalf("fallback content = %q", content)
	}
}

func TestSearchUsesDefaultsAndDoesNotExposeKeyInBody(t *testing.T) {
	client := NewClient(Options{Resolver: fixedResolver("8.8.8.8")})
	var got *http.Request
	var body string
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		got = request.Clone(request.Context())
		encoded, err := ioReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		body = string(encoded)
		return response(request, http.StatusOK, `{"results":[{"title":"A","url":"https://a.example"}]}`, map[string]string{"Content-Type": "application/json"}), nil
	}
	result, err := client.Search(context.Background(), "tvly-secret", "current facts", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.String() != "https://api.tavily.com/search" || got.Header.Get("Authorization") != "Bearer tvly-secret" {
		t.Fatalf("request = %s headers=%v", got.URL, got.Header)
	}
	if strings.Contains(body, "tvly-secret") || !strings.Contains(body, `"max_results":5`) || !strings.Contains(body, `"topic":"general"`) {
		t.Fatalf("body = %s", body)
	}
	if !json.Valid(result) {
		t.Fatalf("result = %s", result)
	}
}

func TestSearchRefusesRedirectAndBoundsOrValidatesResponses(t *testing.T) {
	client := NewClient(Options{Resolver: fixedResolver("8.8.8.8"), MaxResponseBytes: 8})
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		return response(request, http.StatusTemporaryRedirect, "", map[string]string{"Location": "https://attacker.example"}), nil
	}
	_, err := client.Search(context.Background(), "tvly-secret", "query", SearchOptions{})
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("redirect error = %v", err)
	}
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		return response(request, http.StatusOK, `{"long":true}`, nil), nil
	}
	_, err = client.Search(context.Background(), "tvly-secret", "query", SearchOptions{})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("bound error = %v", err)
	}
	client = NewClient(Options{Resolver: fixedResolver("8.8.8.8")})
	client.roundTrip = func(_ context.Context, request *http.Request, _ validatedTarget) (*http.Response, error) {
		return response(request, http.StatusOK, `not json`, nil), nil
	}
	_, err = client.Search(context.Background(), "tvly-secret", "query", SearchOptions{})
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("JSON error = %v", err)
	}
}

func TestSearchValidatesInputWithoutNetwork(t *testing.T) {
	client := NewClient(Options{Resolver: fixedResolver("8.8.8.8")})
	for _, test := range []struct {
		key, query string
		options    SearchOptions
	}{
		{query: "query"},
		{key: "key", query: " "},
		{key: "key", query: "query", options: SearchOptions{MaxResults: 21}},
		{key: "key", query: "query", options: SearchOptions{Topic: "sports"}},
	} {
		if _, err := client.Search(context.Background(), test.key, test.query, test.options); err == nil {
			t.Fatalf("expected validation error for %+v", test)
		}
	}
}

func ioReadAll(reader io.Reader) ([]byte, error) {
	return io.ReadAll(reader)
}
