// Package openrouter adapts OpenRouter's OpenAI-compatible Responses API to
// dago's provider-neutral model contract.
package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/openai"
	"github.com/semistrict/dago/datool"
)

const defaultBaseURL = "https://openrouter.ai/api/v1"

var defaultRetryBackoff = []time.Duration{
	time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
}

// ProviderRouting controls which upstream provider endpoints OpenRouter may use.
// Nil pointer fields preserve OpenRouter's account and service defaults.
type ProviderRouting struct {
	Order                  []string `json:"order,omitempty"`
	Only                   []string `json:"only,omitempty"`
	Ignore                 []string `json:"ignore,omitempty"`
	AllowFallbacks         *bool    `json:"allow_fallbacks,omitempty"`
	RequireParameters      *bool    `json:"require_parameters,omitempty"`
	DataCollection         string   `json:"data_collection,omitempty"`
	ZeroDataRetention      *bool    `json:"zdr,omitempty"`
	Quantizations          []string `json:"quantizations,omitempty"`
	Sort                   string   `json:"sort,omitempty"`
	PreferredMinThroughput float64  `json:"preferred_min_throughput,omitempty"`
	PreferredMaxLatency    float64  `json:"preferred_max_latency,omitempty"`
}

// Options configures an OpenRouter Responses API model.
type Options struct {
	BaseURL         string
	HTTPClient      *http.Client
	MaxOutputTokens int
	ContextWindow   int
	Headers         http.Header
	// AppURL and AppTitle opt the application into OpenRouter attribution.
	// They are sent as HTTP-Referer and X-OpenRouter-Title respectively.
	AppURL   string
	AppTitle string
	// Routing applies OpenRouter's provider selection preferences to every
	// request made by this client.
	Routing *ProviderRouting
	// DefaultReasoning applies when a request does not carry an override.
	DefaultReasoning *damodel.Reasoning
	// Store controls server-side response retention when explicitly set.
	Store *bool
	// WebSearch enables OpenRouter's Responses API web search tool.
	WebSearch bool
	// RetryBackoff controls retries for transport failures, rate limits,
	// server errors, and incomplete JSON responses. Nil selects conservative
	// defaults; an explicitly empty slice disables provider-level retries.
	RetryBackoff []time.Duration
}

// Client is an OpenRouter Responses API-backed chat model.
type Client struct {
	chat damodel.Chat
}

// New creates a model authenticated with an OpenRouter API key. Construction
// does no I/O; missing required values and invalid static options panic.
func New(apiKey, model string, options Options) *Client {
	routing := cloneAndValidateRouting(options.Routing)
	headers := options.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	if options.AppURL != "" {
		headers.Set("HTTP-Referer", options.AppURL)
	}
	if options.AppTitle != "" {
		headers.Set("X-OpenRouter-Title", options.AppTitle)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/responses")

	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if routing != nil {
		copy := *httpClient
		transport := httpClient.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		copy.Transport = routingTransport{next: transport, routing: routing}
		httpClient = &copy
	}
	retryBackoff := options.RetryBackoff
	if retryBackoff == nil {
		retryBackoff = defaultRetryBackoff
	}

	chat := openai.NewCompatibleAPIKey(apiKey, "openrouter", model, openai.Options{
		BaseURL: baseURL, HTTPClient: httpClient,
		MaxOutputTokens: options.MaxOutputTokens, ContextWindow: options.ContextWindow,
		Headers: headers, DefaultReasoning: options.DefaultReasoning, Store: options.Store,
		WebSearch: options.WebSearch, RetryBackoff: retryBackoff,
	})
	return &Client{chat: chat}
}

// BindTools returns an independent client with the supplied tool definitions.
func (client *Client) BindTools(definitions []datool.Definition) (damodel.Chat, error) {
	binder, ok := client.chat.(damodel.Binder)
	if !ok {
		return nil, fmt.Errorf("openrouter: model does not support tool binding")
	}
	bound, err := binder.BindTools(definitions)
	if err != nil {
		return nil, err
	}
	return &Client{chat: bound}, nil
}

func (client *Client) Profile() damodel.Profile {
	return client.chat.Profile()
}

func (client *Client) CountTokens(ctx context.Context, messages []damessage.Message) (int, error) {
	counter, ok := client.chat.(damodel.TokenCounter)
	if !ok {
		return 0, fmt.Errorf("openrouter: model does not support token counting")
	}
	return counter.CountTokens(ctx, messages)
}

func (client *Client) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	return client.chat.Invoke(ctx, request)
}

func (client *Client) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	return client.chat.Stream(ctx, request)
}

func cloneAndValidateRouting(value *ProviderRouting) *ProviderRouting {
	if value == nil {
		return nil
	}
	result := *value
	result.Order = append([]string(nil), value.Order...)
	result.Only = append([]string(nil), value.Only...)
	result.Ignore = append([]string(nil), value.Ignore...)
	result.Quantizations = append([]string(nil), value.Quantizations...)
	if value.AllowFallbacks != nil {
		result.AllowFallbacks = new(*value.AllowFallbacks)
	}
	if value.RequireParameters != nil {
		result.RequireParameters = new(*value.RequireParameters)
	}
	if value.ZeroDataRetention != nil {
		result.ZeroDataRetention = new(*value.ZeroDataRetention)
	}
	for name, items := range map[string][]string{
		"order": result.Order, "only": result.Only, "ignore": result.Ignore, "quantizations": result.Quantizations,
	} {
		for _, item := range items {
			if strings.TrimSpace(item) == "" {
				panic(fmt.Sprintf("openrouter: routing %s contains an empty value", name))
			}
		}
	}
	if result.DataCollection != "" && result.DataCollection != "allow" && result.DataCollection != "deny" {
		panic("openrouter: routing data collection must be allow or deny")
	}
	switch result.Sort {
	case "", "price", "throughput", "latency":
	default:
		panic(fmt.Sprintf("openrouter: routing sort %q is unsupported", result.Sort))
	}
	if result.PreferredMinThroughput < 0 {
		panic("openrouter: preferred minimum throughput cannot be negative")
	}
	if result.PreferredMaxLatency < 0 {
		panic("openrouter: preferred maximum latency cannot be negative")
	}
	return &result
}

type routingTransport struct {
	next    http.RoundTripper
	routing *ProviderRouting
}

func (transport routingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, fmt.Errorf("openrouter: read request body: %w", err)
	}
	_ = request.Body.Close()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("openrouter: decode request body: %w", err)
	}
	routing, err := json.Marshal(transport.routing)
	if err != nil {
		return nil, fmt.Errorf("openrouter: encode provider routing: %w", err)
	}
	payload["provider"] = routing
	body, err = json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("openrouter: encode request body: %w", err)
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return transport.next.RoundTrip(request)
}
