// Package ollama discovers models exposed by an explicitly selected local
// Ollama daemon. It does not construct models or discover credentials.
package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// DefaultEndpoint is the literal-loopback Ollama daemon origin used when the
// positional endpoint is empty.
const DefaultEndpoint = "http://127.0.0.1:11434"

var (
	// ErrInvalidEndpoint marks an endpoint that is not an absolute loopback
	// HTTP(S) origin.
	ErrInvalidEndpoint = errors.New("invalid local Ollama endpoint")
	// ErrUnavailable marks transport failures and non-success responses.
	ErrUnavailable = errors.New("local Ollama discovery unavailable")
	// ErrInvalidResponse marks a malformed daemon response.
	ErrInvalidResponse = errors.New("invalid local Ollama response")
	// ErrLimit marks a response that exceeds a configured finite bound.
	ErrLimit = errors.New("local Ollama discovery limit exceeded")
)

// DiscoveryOptions bounds one local /api/tags request. Zero values select
// finite production defaults.
type DiscoveryOptions struct {
	Timeout          time.Duration
	MaxResponseBytes int64
	MaxModels        int
	MaxNameBytes     int
}

// DefaultDiscoveryOptions returns the finite defaults used by NewDiscovery.
func DefaultDiscoveryOptions() DiscoveryOptions {
	return DiscoveryOptions{
		Timeout: time.Second, MaxResponseBytes: 1 << 20,
		MaxModels: 512, MaxNameBytes: 512,
	}
}

// Discovery is an immutable, local-only model discovery client.
type Discovery struct {
	transport http.RoundTripper
	endpoint  string
	options   DiscoveryOptions
}

// NewDiscovery compiles a local discovery client without performing I/O. The
// transport and endpoint are positional; an empty endpoint selects
// DefaultEndpoint. Invalid static inputs panic.
func NewDiscovery(transport http.RoundTripper, endpoint string, options DiscoveryOptions) *Discovery {
	if nilInterface(transport) {
		panic("ollama: discovery transport is nil")
	}
	defaults := DefaultDiscoveryOptions()
	if options.Timeout == 0 {
		options.Timeout = defaults.Timeout
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = defaults.MaxResponseBytes
	}
	if options.MaxModels == 0 {
		options.MaxModels = defaults.MaxModels
	}
	if options.MaxNameBytes == 0 {
		options.MaxNameBytes = defaults.MaxNameBytes
	}
	if options.Timeout < time.Millisecond || options.Timeout > 30*time.Second ||
		options.MaxResponseBytes < 1 || options.MaxResponseBytes > 16<<20 ||
		options.MaxModels < 1 || options.MaxModels > 4096 ||
		options.MaxNameBytes < 1 || options.MaxNameBytes > 4096 {
		panic("ollama: discovery options are outside their finite bounds")
	}
	normalized, err := normalizeEndpoint(endpoint)
	if err != nil {
		panic(err)
	}
	return &Discovery{transport: transport, endpoint: normalized, options: options}
}

// Endpoint returns the normalized literal-loopback endpoint.
func (discovery *Discovery) Endpoint() string { return discovery.endpoint }

// Discover explicitly probes /api/tags and returns sorted, unique model names.
// It performs exactly one request and does not cache, retry, follow redirects,
// or attach credentials. Callers choose when discovery and refreshes occur.
func (discovery *Discovery) Discover(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, discovery.options.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, discovery.endpoint+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrInvalidEndpoint)
	}
	request.Header.Set("Accept", "application/json")
	response, err := roundTrip(discovery.transport, request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: request failed", ErrUnavailable)
	}
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("%w: transport returned an empty response", ErrInvalidResponse)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP status %d", ErrUnavailable, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, discovery.options.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response", ErrUnavailable)
	}
	if int64(len(body)) > discovery.options.MaxResponseBytes {
		return nil, fmt.Errorf("%w: response exceeds %d bytes", ErrLimit, discovery.options.MaxResponseBytes)
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Models == nil {
		return nil, fmt.Errorf("%w: expected a models array", ErrInvalidResponse)
	}
	if len(payload.Models) > discovery.options.MaxModels {
		return nil, fmt.Errorf("%w: response exceeds %d models", ErrLimit, discovery.options.MaxModels)
	}
	seen := make(map[string]struct{}, len(payload.Models))
	models := make([]string, 0, len(payload.Models))
	for _, model := range payload.Models {
		if !validModelName(model.Name, discovery.options.MaxNameBytes) {
			return nil, fmt.Errorf("%w: model name is empty, padded, unsafe, or too long", ErrInvalidResponse)
		}
		if _, duplicate := seen[model.Name]; duplicate {
			continue
		}
		seen[model.Name] = struct{}{}
		models = append(models, model.Name)
	}
	sort.Strings(models)
	return models, nil
}

func normalizeEndpoint(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = DefaultEndpoint
	}
	if len(value) > 4096 || strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("%w: endpoint is empty or too long", ErrInvalidEndpoint)
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" ||
		parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("%w: expected an HTTP(S) loopback origin", ErrInvalidEndpoint)
	}
	host := parsed.Hostname()
	if host == "localhost" {
		host = "127.0.0.1"
	} else {
		address := net.ParseIP(host)
		if address == nil || !address.IsLoopback() {
			return "", fmt.Errorf("%w: host must be a literal loopback address", ErrInvalidEndpoint)
		}
	}
	port := parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("%w: port is invalid", ErrInvalidEndpoint)
		}
	}
	parsed.Host = net.JoinHostPort(host, port)
	if port == "" {
		parsed.Host = host
		if strings.Contains(host, ":") {
			parsed.Host = "[" + host + "]"
		}
	}
	parsed.Path, parsed.RawPath = "", ""
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func validModelName(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || value != strings.TrimSpace(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func roundTrip(transport http.RoundTripper, request *http.Request) (response *http.Response, err error) {
	defer func() {
		if recover() != nil {
			response, err = nil, errors.New("discovery transport panicked")
		}
	}()
	return transport.RoundTrip(request)
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
