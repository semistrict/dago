package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Transport is the narrow authenticated JSON contract exposed by the Node
// bridge. Implementations must honor ctx and bound returned payloads.
type Transport interface {
	Get(context.Context, string) (json.RawMessage, error)
	Post(context.Context, string, any) (json.RawMessage, error)
}

// HTTPOptions controls the built-in loopback transport. Zero values select a
// ten-second request deadline and 1 MiB request/response bounds.
type HTTPOptions struct {
	Timeout          time.Duration
	MaxRequestBytes  int64
	MaxResponseBytes int64
}

// HTTPTransport is an authenticated, DNS-free, loopback-only bridge client.
type HTTPTransport struct {
	baseURL    string
	token      string
	client     *http.Client
	timeout    time.Duration
	maxRequest int64
	maxReply   int64
}

// NewHTTPTransport constructs a bridge transport. baseURL and bearerToken are
// required positional static inputs; invalid values panic.
func NewHTTPTransport(baseURL, bearerToken string, options HTTPOptions) *HTTPTransport {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	bearerToken = strings.TrimSpace(bearerToken)
	if bearerToken == "" {
		panic("whatsapp bridge transport: bearer token is required")
	}
	parsed, dialAddress := loopbackAddress(baseURL)
	if options.Timeout < 0 || options.MaxRequestBytes < 0 || options.MaxResponseBytes < 0 {
		panic("whatsapp bridge transport: limits cannot be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = 10 * time.Second
	}
	if options.MaxRequestBytes == 0 {
		options.MaxRequestBytes = defaultMaxBridgeBytes
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = defaultMaxBridgeBytes
	}
	dialer := &net.Dialer{Timeout: options.Timeout}
	roundTripper := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, dialAddress)
		},
		DisableCompression: true,
	}
	client := &http.Client{
		Transport: roundTripper,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("whatsapp bridge redirects are disabled")
		},
	}
	return &HTTPTransport{
		baseURL: parsed.String(), token: bearerToken, client: client,
		timeout: options.Timeout, maxRequest: options.MaxRequestBytes, maxReply: options.MaxResponseBytes,
	}
}

func loopbackAddress(value string) (*url.URL, string) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		panic("whatsapp bridge transport: URL must be an HTTP loopback origin")
	}
	hostname := strings.ToLower(parsed.Hostname())
	var literal string
	switch hostname {
	case "127.0.0.1", "localhost":
		literal = "127.0.0.1"
	case "::1":
		literal = "::1"
	default:
		panic("whatsapp bridge transport: URL must be an HTTP loopback origin")
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
	}
	parsed.Path = ""
	return parsed, net.JoinHostPort(literal, port)
}

// Get implements Transport.
func (transport *HTTPTransport) Get(ctx context.Context, endpoint string) (json.RawMessage, error) {
	return transport.request(ctx, http.MethodGet, endpoint, nil)
}

// Post implements Transport.
func (transport *HTTPTransport) Post(ctx context.Context, endpoint string, payload any) (json.RawMessage, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode WhatsApp bridge request: %w", err)
	}
	if int64(len(encoded)) > transport.maxRequest {
		return nil, ErrBridgePayloadTooLarge
	}
	return transport.request(ctx, http.MethodPost, endpoint, encoded)
}

func (transport *HTTPTransport) request(ctx context.Context, method, endpoint string, body []byte) (json.RawMessage, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if !validEndpoint(endpoint) {
		return nil, errors.New("whatsapp bridge endpoint is invalid")
	}
	requestCtx, cancel := context.WithTimeout(ctx, transport.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, transport.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build WhatsApp bridge request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+transport.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := transport.client.Do(request)
	if requestErr := requestCtx.Err(); requestErr != nil {
		return nil, requestErr
	}
	if err != nil {
		return nil, fmt.Errorf("WhatsApp bridge request: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, transport.maxReply+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read WhatsApp bridge response: %w", err)
	}
	if int64(len(payload)) > transport.maxReply {
		return nil, ErrBridgePayloadTooLarge
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, bridgeHTTPError{status: response.StatusCode, detail: bridgeErrorDetail(payload)}
	}
	if !json.Valid(payload) {
		return nil, errors.New("WhatsApp bridge returned invalid JSON")
	}
	return append(json.RawMessage(nil), payload...), nil
}

func validEndpoint(endpoint string) bool {
	if endpoint == "" || endpoint[0] != '/' || strings.HasPrefix(endpoint, "//") || strings.ContainsAny(endpoint, "?#\\") {
		return false
	}
	parsed, err := url.ParseRequestURI(endpoint)
	return err == nil && parsed.Path == endpoint
}

type bridgeHTTPError struct {
	status int
	detail string
}

func (err bridgeHTTPError) Error() string {
	if err.detail == "" {
		return "WhatsApp bridge returned HTTP " + strconv.Itoa(err.status)
	}
	return "WhatsApp bridge returned HTTP " + strconv.Itoa(err.status) + ": " + err.detail
}

func bridgeErrorDetail(payload []byte) string {
	var response struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(payload, &response) == nil {
		return boundString(response.Error, defaultMaxErrorBytes)
	}
	return ""
}
