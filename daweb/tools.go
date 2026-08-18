package daweb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/semistrict/dago/datool"
)

type httpRequestInput struct {
	URL            string            `json:"url" description:"HTTP or HTTPS URL to request" jsonschema:"minLength=1"`
	Method         string            `json:"method,omitempty" description:"HTTP method; defaults to GET" jsonschema:"enum=GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS,default=GET"`
	Headers        map[string]string `json:"headers,omitempty" description:"Optional request headers"`
	Body           string            `json:"body,omitempty" description:"Optional UTF-8 request body"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" description:"Optional timeout in seconds; defaults to 30 and cannot exceed the client maximum" jsonschema:"minimum=1"`
}

// NewHTTPRequestTool exposes bounded HTTP requests through client. Supplying the
// client positionally makes the network authority explicit at the call site.
func NewHTTPRequestTool(client *Client) datool.Tool {
	requireClient(client)
	return datool.MustNew("http_request", "Make a bounded HTTP or HTTPS request. Private, reserved, local, and rebinding targets are blocked.",
		func(ctx context.Context, input httpRequestInput) (Response, error) {
			timeout, err := client.toolTimeout(input.TimeoutSeconds)
			if err != nil {
				return Response{}, err
			}
			return client.Do(ctx, input.URL, Request{
				Method: input.Method, Headers: input.Headers, Body: input.Body,
				Timeout: timeout,
			})
		})
}

type fetchURLInput struct {
	URL            string `json:"url" description:"HTTP or HTTPS page URL to fetch" jsonschema:"minLength=1"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" description:"Optional timeout in seconds; defaults to 30 and cannot exceed the client maximum" jsonschema:"minimum=1"`
}

// FetchResult is the stable result returned by fetch_url.
type FetchResult struct {
	URL             string `json:"url"`
	MarkdownContent string `json:"markdown_content"`
	StatusCode      int    `json:"status_code"`
	ContentLength   int    `json:"content_length"`
	Truncated       bool   `json:"truncated,omitempty"`
}

// NewFetchURLTool exposes bounded page fetching and HTML-to-Markdown conversion.
func NewFetchURLTool(client *Client) datool.Tool {
	requireClient(client)
	return datool.MustNew("fetch_url", "Fetch a public HTTP or HTTPS page and return bounded Markdown content. Every redirect and resolved address is checked.",
		func(ctx context.Context, input fetchURLInput) (FetchResult, error) {
			timeout, err := client.toolTimeout(input.TimeoutSeconds)
			if err != nil {
				return FetchResult{}, err
			}
			response, err := client.Do(ctx, input.URL, Request{Timeout: timeout})
			if err != nil {
				return FetchResult{}, err
			}
			content := response.Body
			if mediaType := strings.ToLower(responseHeader(response.Headers, "Content-Type")); strings.Contains(mediaType, "html") || looksLikeHTML(content) {
				content = htmlToMarkdown(content)
			}
			content, truncated := truncateUTF8(content, client.configured().maxRenderedBytes)
			return FetchResult{
				URL: response.URL, MarkdownContent: content, StatusCode: response.StatusCode,
				ContentLength: len(content), Truncated: truncated,
			}, nil
		})
}

// SearchOptions controls a Tavily query. Zero values select five general results
// without raw page content.
type SearchOptions struct {
	MaxResults        int    `json:"max_results,omitempty"`
	Topic             string `json:"topic,omitempty"`
	IncludeRawContent bool   `json:"include_raw_content,omitempty"`
}

type webSearchInput struct {
	Query             string `json:"query" description:"Specific and detailed search query" jsonschema:"minLength=1"`
	MaxResults        int    `json:"max_results,omitempty" description:"Number of results; defaults to 5" jsonschema:"minimum=1,maximum=20,default=5"`
	Topic             string `json:"topic,omitempty" description:"Search topic; defaults to general" jsonschema:"enum=general|news|finance,default=general"`
	IncludeRawContent bool   `json:"include_raw_content,omitempty" description:"Include full page content; prefer fetch_url for a single URL"`
}

// NewWebSearchTool exposes Tavily search. The client and secret are required
// positional inputs; the tool is therefore absent unless an application opts in.
func NewWebSearchTool(client *Client, tavilyAPIKey string) datool.Tool {
	requireClient(client)
	if strings.TrimSpace(tavilyAPIKey) == "" {
		panic("daweb: Tavily API key is required")
	}
	return datool.MustNew("web_search", "Search the web with Tavily for current information.",
		func(ctx context.Context, input webSearchInput) (json.RawMessage, error) {
			return client.Search(ctx, tavilyAPIKey, input.Query, SearchOptions{
				MaxResults: input.MaxResults, Topic: input.Topic,
				IncludeRawContent: input.IncludeRawContent,
			})
		})
}

// Search performs a bounded Tavily query. Redirects are refused so the API key
// cannot be forwarded to a different origin.
func (client *Client) Search(ctx context.Context, tavilyAPIKey, query string, options SearchOptions) (json.RawMessage, error) {
	client.configured()
	if strings.TrimSpace(tavilyAPIKey) == "" {
		return nil, fmt.Errorf("Tavily API key is required")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("Tavily query is required")
	}
	if options.MaxResults == 0 {
		options.MaxResults = 5
	}
	if options.MaxResults < 1 || options.MaxResults > 20 {
		return nil, fmt.Errorf("Tavily max results must be between 1 and 20")
	}
	if options.Topic == "" {
		options.Topic = "general"
	}
	if options.Topic != "general" && options.Topic != "news" && options.Topic != "finance" {
		return nil, fmt.Errorf("Tavily topic must be general, news, or finance")
	}
	payload, err := json.Marshal(map[string]any{
		"query": query, "max_results": options.MaxResults, "topic": options.Topic,
		"include_raw_content": options.IncludeRawContent,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Tavily request: %w", err)
	}
	response, err := client.do(ctx, "https://api.tavily.com/search", Request{
		Method: http.MethodPost,
		Headers: map[string]string{
			"Authorization": "Bearer " + tavilyAPIKey,
			"Content-Type":  "application/json",
			"Accept":        "application/json",
		},
		Body: string(payload),
	}, false)
	if err != nil {
		return nil, fmt.Errorf("Tavily search: %w", err)
	}
	if !json.Valid([]byte(response.Body)) {
		return nil, fmt.Errorf("Tavily search returned invalid JSON")
	}
	return json.RawMessage(response.Body), nil
}

// Tools returns the always-available HTTP tools, adding web_search only when a
// non-empty Tavily API key is explicitly supplied.
func Tools(client *Client, tavilyAPIKey string) []datool.Tool {
	requireClient(client)
	tools := []datool.Tool{NewHTTPRequestTool(client), NewFetchURLTool(client)}
	if strings.TrimSpace(tavilyAPIKey) != "" {
		tools = append(tools, NewWebSearchTool(client, tavilyAPIKey))
	}
	return tools
}

func requireClient(client *Client) {
	if client == nil {
		panic("daweb: client is required")
	}
}

func (client *Client) toolTimeout(value int) (time.Duration, error) {
	if value == 0 {
		return 0, nil
	}
	maximum := client.configured().maxTimeout / time.Second
	if value < 1 || int64(value) > int64(maximum) {
		return 0, fmt.Errorf("timeout_seconds must be between 1 and %d", maximum)
	}
	return time.Duration(value) * time.Second, nil
}

func responseHeader(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
