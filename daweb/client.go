// Package daweb provides opt-in HTTP, page-fetching, and Tavily search tools.
//
// Every model-supplied URL is resolved before use, every resolved address must be
// globally routable, and the connection is pinned to the validated addresses.
// Redirect destinations are subjected to the same checks. Environment proxies are
// deliberately ignored because they would resolve the target outside this boundary.
package daweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/idna"
)

var (
	ErrInvalidURL       = errors.New("invalid web URL")
	ErrBlockedAddress   = errors.New("web address is not globally routable")
	ErrResponseTooLarge = errors.New("web response exceeds configured limit")
	ErrTooManyRedirects = errors.New("too many web redirects")
	ErrHTTPStatus       = errors.New("web request returned an unsuccessful status")
)

const (
	defaultTimeout          = 30 * time.Second
	defaultMaxTimeout       = 60 * time.Second
	defaultMaxRedirects     = 5
	defaultMaxResponseBytes = 2 << 20
	defaultMaxRequestBytes  = 256 << 10
	defaultMaxRenderedBytes = 256 << 10
	defaultUserAgent        = "dago-web/1.0"
)

// Resolver is the DNS capability used at the SSRF validation boundary.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// Options configures finite resource bounds. Zero fields select secure defaults.
// Resolver is optional and defaults to net.DefaultResolver.
type Options struct {
	Resolver         Resolver
	Timeout          time.Duration
	MaxTimeout       time.Duration
	MaxRedirects     int
	MaxResponseBytes int64
	MaxRequestBytes  int64
	MaxRenderedBytes int
	UserAgent        string
}

type clientConfig struct {
	resolver         Resolver
	timeout          time.Duration
	maxTimeout       time.Duration
	maxRedirects     int
	maxResponseBytes int64
	maxRequestBytes  int64
	maxRenderedBytes int
	userAgent        string
}

// Client owns the network authority used by web tools. Its zero value is ready
// for use with the same secure defaults as NewClient(Options{}).
type Client struct {
	config    clientConfig
	roundTrip func(context.Context, *http.Request, validatedTarget) (*http.Response, error)
}

// NewClient compiles immutable network policy and resource bounds. Invalid static
// limits panic; operational DNS and HTTP failures are returned by request methods.
func NewClient(options Options) *Client {
	if options.Timeout < 0 || options.MaxTimeout < 0 || options.MaxRedirects < 0 ||
		options.MaxResponseBytes < 0 || options.MaxRequestBytes < 0 || options.MaxRenderedBytes < 0 {
		panic("daweb: limits must not be negative")
	}
	if nilResolver(options.Resolver) {
		options.Resolver = nil
	}
	config := clientConfig{
		resolver:         options.Resolver,
		timeout:          options.Timeout,
		maxTimeout:       options.MaxTimeout,
		maxRedirects:     options.MaxRedirects,
		maxResponseBytes: options.MaxResponseBytes,
		maxRequestBytes:  options.MaxRequestBytes,
		maxRenderedBytes: options.MaxRenderedBytes,
		userAgent:        strings.TrimSpace(options.UserAgent),
	}
	config.setDefaults()
	return &Client{config: config}
}

func nilResolver(resolver Resolver) bool {
	if resolver == nil {
		return true
	}
	value := reflect.ValueOf(resolver)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (config *clientConfig) setDefaults() {
	if config.resolver == nil {
		config.resolver = net.DefaultResolver
	}
	if config.maxTimeout == 0 {
		config.maxTimeout = defaultMaxTimeout
	}
	if config.timeout == 0 {
		config.timeout = min(defaultTimeout, config.maxTimeout)
	}
	if config.timeout > config.maxTimeout {
		panic("daweb: Timeout must not exceed MaxTimeout")
	}
	if config.maxRedirects == 0 {
		config.maxRedirects = defaultMaxRedirects
	}
	if config.maxResponseBytes == 0 {
		config.maxResponseBytes = defaultMaxResponseBytes
	}
	if config.maxRequestBytes == 0 {
		config.maxRequestBytes = defaultMaxRequestBytes
	}
	if config.maxRenderedBytes == 0 {
		config.maxRenderedBytes = defaultMaxRenderedBytes
	}
	if config.userAgent == "" {
		config.userAgent = defaultUserAgent
	}
}

func (client *Client) configured() clientConfig {
	if client == nil {
		panic("daweb: client is required")
	}
	config := client.config
	config.setDefaults()
	return config
}

// Request contains optional HTTP request settings. Method defaults to GET.
type Request struct {
	Method  string
	Headers map[string]string
	Body    string
	Timeout time.Duration
}

// Response is a bounded HTTP response.
type Response struct {
	URL        string              `json:"url"`
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       string              `json:"body"`
}

// Do performs a request to rawURL through the SSRF-hardened network boundary.
func (client *Client) Do(ctx context.Context, rawURL string, request Request) (Response, error) {
	return client.do(ctx, rawURL, request, true)
}

func (client *Client) do(ctx context.Context, rawURL string, request Request, followRedirects bool) (Response, error) {
	config := client.configured()
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		return Response{}, fmt.Errorf("%w: HTTP method %q is not allowed", ErrInvalidURL, method)
	}
	requestBytes := int64(len(rawURL) + len(request.Body))
	for name, value := range request.Headers {
		requestBytes += int64(len(name) + len(value))
	}
	if requestBytes > config.maxRequestBytes {
		return Response{}, fmt.Errorf("request: %w (%d bytes, limit %d)", ErrResponseTooLarge, requestBytes, config.maxRequestBytes)
	}
	timeout := request.Timeout
	if timeout == 0 {
		timeout = config.timeout
	}
	if timeout < 0 || timeout > config.maxTimeout {
		return Response{}, fmt.Errorf("request timeout must be between 1ns and %s", config.maxTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	headers, err := validatedHeaders(request.Headers)
	if err != nil {
		return Response{}, err
	}
	body := request.Body
	current := rawURL
	for redirects := 0; ; redirects++ {
		target, validationErr := validateTarget(ctx, config.resolver, current)
		if validationErr != nil {
			return Response{}, validationErr
		}
		httpRequest, requestErr := http.NewRequestWithContext(ctx, method, target.url.String(), bytes.NewBufferString(body))
		if requestErr != nil {
			return Response{}, fmt.Errorf("%w: %v", ErrInvalidURL, requestErr)
		}
		httpRequest.Header = headers.Clone()
		if httpRequest.Header.Get("User-Agent") == "" {
			httpRequest.Header.Set("User-Agent", config.userAgent)
		}
		response, requestErr := client.send(ctx, httpRequest, target)
		if requestErr != nil {
			return Response{}, requestErr
		}

		if response.StatusCode < 300 || response.StatusCode > 399 {
			return readResponse(response, config.maxResponseBytes)
		}
		location := response.Header.Get("Location")
		response.Body.Close()
		if !followRedirects {
			return Response{}, fmt.Errorf("%w: redirects are disabled", ErrHTTPStatus)
		}
		if location == "" {
			return Response{}, fmt.Errorf("%w: status %d is missing Location", ErrInvalidURL, response.StatusCode)
		}
		if redirects >= config.maxRedirects {
			return Response{}, fmt.Errorf("%w: limit %d", ErrTooManyRedirects, config.maxRedirects)
		}
		nextURL, parseErr := target.url.Parse(location)
		if parseErr != nil {
			return Response{}, fmt.Errorf("%w: redirect Location: %v", ErrInvalidURL, parseErr)
		}
		if !sameOrigin(target.url, nextURL) {
			if body != "" && response.StatusCode != http.StatusSeeOther &&
				!((response.StatusCode == http.StatusMovedPermanently || response.StatusCode == http.StatusFound) && method == http.MethodPost) {
				return Response{}, fmt.Errorf("%w: cross-origin redirect with a request body is not allowed", ErrInvalidURL)
			}
			stripCrossOriginHeaders(headers)
		}
		if response.StatusCode == http.StatusSeeOther ||
			((response.StatusCode == http.StatusMovedPermanently || response.StatusCode == http.StatusFound) && method == http.MethodPost) {
			method = http.MethodGet
			body = ""
			headers.Del("Content-Length")
		}
		current = nextURL.String()
	}
}

func (client *Client) send(ctx context.Context, request *http.Request, target validatedTarget) (*http.Response, error) {
	if client.roundTrip != nil {
		return client.roundTrip(ctx, request, target)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            pinnedDialContext(dialer.DialContext, target),
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DisableKeepAlives:      true,
	}
	defer transport.CloseIdleConnections()
	return transport.RoundTrip(request)
}

func readResponse(response *http.Response, limit int64) (Response, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return Response{}, fmt.Errorf("read web response: %w", err)
	}
	if int64(len(body)) > limit {
		return Response{}, fmt.Errorf("%w (%d byte limit)", ErrResponseTooLarge, limit)
	}
	result := Response{
		URL:        response.Request.URL.String(),
		StatusCode: response.StatusCode,
		Headers:    response.Header.Clone(),
		Body:       strings.ToValidUTF8(string(body), "�"),
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, fmt.Errorf("%w: status %d", ErrHTTPStatus, response.StatusCode)
	}
	return result, nil
}

type validatedTarget struct {
	url  *url.URL
	host string
	ips  []netip.Addr
}

func validateTarget(ctx context.Context, resolver Resolver, rawURL string) (validatedTarget, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return validatedTarget{}, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return validatedTarget{}, fmt.Errorf("%w: scheme must be http or https", ErrInvalidURL)
	}
	if parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" {
		return validatedTarget{}, fmt.Errorf("%w: hostname is required", ErrInvalidURL)
	}
	if parsed.User != nil {
		return validatedTarget{}, fmt.Errorf("%w: URL credentials are not allowed", ErrInvalidURL)
	}
	if parsed.Fragment != "" {
		parsed.Fragment = ""
	}
	port := parsed.Port()
	if port != "" {
		number, parseErr := strconv.Atoi(port)
		if parseErr != nil || number < 1 || number > 65535 {
			return validatedTarget{}, fmt.Errorf("%w: invalid port", ErrInvalidURL)
		}
	}
	host := parsed.Hostname()
	if index := strings.LastIndexByte(host, '%'); index >= 0 {
		host = host[:index]
	}
	literal, literalErr := netip.ParseAddr(host)
	asciiHost := ""
	if literalErr == nil {
		asciiHost = literal.String()
	} else {
		asciiHost, err = idna.Lookup.ToASCII(host)
		if err != nil || asciiHost == "" {
			return validatedTarget{}, fmt.Errorf("%w: invalid hostname", ErrInvalidURL)
		}
	}
	asciiHost = strings.ToLower(strings.TrimSuffix(asciiHost, "."))
	if asciiHost == "localhost" || strings.HasSuffix(asciiHost, ".localhost") {
		return validatedTarget{}, fmt.Errorf("%w: localhost", ErrBlockedAddress)
	}
	var addresses []netip.Addr
	if literalErr == nil {
		addresses = []netip.Addr{literal}
	} else {
		addresses, err = resolver.LookupNetIP(ctx, "ip", asciiHost)
		if err != nil {
			if ctx.Err() != nil {
				return validatedTarget{}, fmt.Errorf("resolve hostname: %w", ctx.Err())
			}
			return validatedTarget{}, fmt.Errorf("%w: resolve hostname: %v", ErrInvalidURL, err)
		}
	}
	if len(addresses) == 0 {
		return validatedTarget{}, fmt.Errorf("%w: hostname resolved to no addresses", ErrInvalidURL)
	}
	seen := map[netip.Addr]bool{}
	validated := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if isBlockedAddress(address) {
			return validatedTarget{}, fmt.Errorf("%w: %s", ErrBlockedAddress, address)
		}
		if !seen[address] {
			seen[address] = true
			validated = append(validated, address)
		}
	}
	parsed.Host = asciiHost
	if port != "" {
		parsed.Host = net.JoinHostPort(asciiHost, port)
	} else if strings.Contains(asciiHost, ":") {
		parsed.Host = "[" + asciiHost + "]"
	}
	return validatedTarget{url: parsed, host: asciiHost, ips: validated}, nil
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("ff00::/8"),
}

func isBlockedAddress(address netip.Addr) bool {
	if !address.IsValid() {
		return true
	}
	if address.Is4In6() {
		address = address.Unmap()
	}
	if address.Is6() && netip.MustParsePrefix("2002::/16").Contains(address) {
		bytes := address.As16()
		address = netip.AddrFrom4([4]byte{bytes[2], bytes[3], bytes[4], bytes[5]})
	}
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return true
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func pinnedDialContext(dial dialContextFunc, target validatedTarget) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("pinned web dial: %w", err)
		}
		host = strings.Trim(strings.ToLower(host), "[]")
		if host != target.host {
			return nil, fmt.Errorf("pinned web dial: unexpected hostname %q", host)
		}
		var failures []error
		for _, address := range target.ips {
			connection, dialErr := dial(ctx, network, net.JoinHostPort(address.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			failures = append(failures, dialErr)
		}
		return nil, fmt.Errorf("pinned web dial: %w", errors.Join(failures...))
	}
}

func validatedHeaders(values map[string]string) (http.Header, error) {
	headers := make(http.Header, len(values))
	for name, value := range values {
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if canonical == "" || strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("invalid HTTP header %q", name)
		}
		switch canonical {
		case "Host", "Connection", "Proxy-Connection", "Transfer-Encoding", "Upgrade":
			return nil, fmt.Errorf("HTTP header %q is not allowed", canonical)
		}
		headers.Set(canonical, value)
	}
	return headers, nil
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func stripCrossOriginHeaders(headers http.Header) {
	for name := range headers {
		switch http.CanonicalHeaderKey(name) {
		case "Accept", "Accept-Encoding", "Accept-Language", "User-Agent":
		default:
			headers.Del(name)
		}
	}
}
