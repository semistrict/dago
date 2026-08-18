// Package oauthpolicy selects bounded, provider-specific OAuth policies for
// remote MCP servers. It contains no browser, terminal, or hosted-service UI.
package oauthpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/semistrict/dago/datalon/mcp"
	"golang.org/x/oauth2"
)

const (
	genericRedirectURL = "http://127.0.0.1:53682/callback"
	slackRedirectURL   = "http://localhost:3118/callback"
	slackClientID      = "4518649543379.10944517634130"
	githubClientID     = "Iv23libxz8qOApH0WQL3"
	githubDeviceURL    = "https://github.com/login/device/code"
	githubTokenURL     = "https://github.com/login/oauth/access_token"
	maximumTimeout     = 5 * time.Minute
	maximumBodyBytes   = 4 << 20
	maximumCallback    = 64 << 10
	maximumToken       = 256 << 10
	maximumDeviceAge   = time.Hour
	maximumPollDelay   = 5 * time.Minute
	maximumPollCount   = 3600
)

// Provider is the selected OAuth protocol policy.
type Provider string

const (
	ProviderGeneric Provider = "generic"
	ProviderSlack   Provider = "slack"
	ProviderGitHub  Provider = "github"
)

var (
	ErrAuthorizationDenied = errors.New("OAuth authorization was denied")
	ErrDeviceCode          = errors.New("OAuth device authorization failed")
	ErrInteraction         = errors.New("OAuth interaction failed")
	ErrInvalidResponse     = errors.New("OAuth provider response is invalid")
	ErrTokenStore          = errors.New("OAuth token store failed")
	workspacePattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// WorkspaceSelector is an optional interaction capability. Returning an empty
// workspace lets Slack show its normal workspace chooser.
type WorkspaceSelector interface {
	SelectSlackWorkspace(context.Context) (string, error)
}

// DeviceCode contains the public instructions needed to complete a device
// flow. DeviceCode itself is intentionally omitted because it is a credential.
type DeviceCode struct {
	UserCode        string
	VerificationURI string
	ExpiresIn       time.Duration
}

// DeviceCodePresenter is required by the GitHub policy.
type DeviceCodePresenter interface {
	PresentDeviceCode(context.Context, DeviceCode) error
}

// Options sets finite resource bounds. Its zero value is useful and safe.
type Options struct {
	RequestTimeout    time.Duration
	MaxResponseBytes  int64
	MaxCallbackBytes  int
	MaxTokenBytes     int
	MaxDeviceLifetime time.Duration
	MinPollInterval   time.Duration
	MaxPollInterval   time.Duration
	MaxPolls          int
}

type limits struct {
	requestTimeout    time.Duration
	maxResponseBytes  int64
	maxCallbackBytes  int
	maxTokenBytes     int
	maxDeviceLifetime time.Duration
	minPollInterval   time.Duration
	maxPollInterval   time.Duration
	maxPolls          int
}

func normalize(options Options) limits {
	if options.RequestTimeout < 0 || options.RequestTimeout > maximumTimeout ||
		options.MaxResponseBytes < 0 || options.MaxResponseBytes > maximumBodyBytes ||
		options.MaxCallbackBytes < 0 || options.MaxCallbackBytes > maximumCallback ||
		options.MaxTokenBytes < 0 || options.MaxTokenBytes > maximumToken ||
		options.MaxDeviceLifetime < 0 || options.MaxDeviceLifetime > maximumDeviceAge ||
		options.MinPollInterval < 0 || options.MinPollInterval > maximumPollDelay ||
		options.MaxPollInterval < 0 || options.MaxPollInterval > maximumPollDelay ||
		options.MaxPolls < 0 || options.MaxPolls > maximumPollCount {
		panic("OAuth policy option exceeds its hard limit")
	}
	result := limits{
		requestTimeout: 30 * time.Second, maxResponseBytes: 256 << 10,
		maxCallbackBytes: 16 << 10, maxTokenBytes: 64 << 10,
		maxDeviceLifetime: 15 * time.Minute, minPollInterval: time.Second,
		maxPollInterval: 30 * time.Second, maxPolls: 900,
	}
	if options.RequestTimeout > 0 {
		result.requestTimeout = options.RequestTimeout
	}
	if options.MaxResponseBytes > 0 {
		result.maxResponseBytes = options.MaxResponseBytes
	}
	if options.MaxCallbackBytes > 0 {
		result.maxCallbackBytes = options.MaxCallbackBytes
	}
	if options.MaxTokenBytes > 0 {
		result.maxTokenBytes = options.MaxTokenBytes
	}
	if options.MaxDeviceLifetime > 0 {
		result.maxDeviceLifetime = options.MaxDeviceLifetime
	}
	if options.MinPollInterval > 0 {
		result.minPollInterval = options.MinPollInterval
	}
	if options.MaxPollInterval > 0 {
		result.maxPollInterval = options.MaxPollInterval
	}
	if options.MaxPolls > 0 {
		result.maxPolls = options.MaxPolls
	}
	if result.minPollInterval > result.maxPollInterval {
		panic("minimum OAuth poll interval exceeds maximum")
	}
	return result
}

// Select returns the policy for an MCP endpoint. Matching is hostname-based,
// never substring-based, so attacker-controlled lookalike domains fall back to
// the standards-based generic policy.
func Select(endpoint string) Provider {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ProviderGeneric
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case host == "slack.com" || strings.HasSuffix(host, ".slack.com"):
		return ProviderSlack
	case host == "api.githubcopilot.com":
		return ProviderGitHub
	default:
		return ProviderGeneric
	}
}

// Service is an MCP OAuth handler for one statically configured server.
type Service struct {
	client      *http.Client
	store       mcp.TokenStore
	interaction mcp.Interaction
	serverName  string
	endpoint    string
	provider    Provider
	key         string
	limits      limits

	gate  chan struct{}
	mu    sync.Mutex
	auth  auth.OAuthHandler
	token *oauth2.Token
	wait  func(context.Context, time.Duration) error
}

// New constructs a service without performing network or storage I/O.
func New(httpClient *http.Client, store mcp.TokenStore, interaction mcp.Interaction, serverName string, server mcp.Server, options Options) *Service {
	if httpClient == nil || isNil(store) || isNil(interaction) {
		panic("OAuth HTTP client, token store, and interaction are required")
	}
	if strings.TrimSpace(serverName) == "" || strings.ContainsAny(serverName, "\x00\r\n") {
		panic("OAuth server name is required")
	}
	endpoint, err := strictRemoteURL(server.URL)
	if err != nil {
		panic(err)
	}
	if server.Auth != "oauth" {
		panic("MCP server must use OAuth")
	}
	configured := normalize(options)
	clientCopy := *httpClient
	base := clientCopy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clientCopy.Transport = &boundedTransport{base: base, maximum: configured.maxResponseBytes}
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if clientCopy.Timeout == 0 || clientCopy.Timeout > configured.requestTimeout {
		clientCopy.Timeout = configured.requestTimeout
	}
	digest := sha256.Sum256([]byte(serverName + "\x00" + endpoint))
	service := &Service{
		client: &clientCopy, store: &safeStore{delegate: store, maximum: configured.maxTokenBytes},
		interaction: interaction, serverName: serverName, endpoint: endpoint,
		provider: Select(endpoint), key: hex.EncodeToString(digest[:]), limits: configured,
		gate: make(chan struct{}, 1),
	}
	service.gate <- struct{}{}
	service.wait = waitContext
	return service
}

// NewFactory binds the required host dependencies for MCP client setup.
func NewFactory(httpClient *http.Client, store mcp.TokenStore, interaction mcp.Interaction, options Options) mcp.OAuthFactory {
	if httpClient == nil || isNil(store) || isNil(interaction) {
		panic("OAuth HTTP client, token store, and interaction are required")
	}
	return func(serverName string, server mcp.Server) (auth.OAuthHandler, error) {
		return New(httpClient, store, interaction, serverName, server, options), nil
	}
}

// Provider reports the selected protocol without I/O.
func (service *Service) Provider() Provider {
	service.requireInitialized()
	return service.provider
}

func (service *Service) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	service.requireInitialized()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if service.provider == ProviderGitHub {
		return service.githubTokenSource(ctx)
	}
	handler, err := service.authorizationCodeHandler(ctx)
	if err != nil {
		return nil, err
	}
	return handler.TokenSource(ctx)
}

func (service *Service) Authorize(ctx context.Context, request *http.Request, response *http.Response) error {
	service.requireInitialized()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-service.gate:
	}
	defer func() { service.gate <- struct{}{} }()
	if service.provider == ProviderGitHub {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return service.authorizeGitHub(ctx)
	}
	if request == nil || response == nil {
		return ErrInvalidResponse
	}
	handler, err := service.authorizationCodeHandler(ctx)
	if err != nil {
		return err
	}
	if err := handler.Authorize(ctx, request, response); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("OAuth authorization failed")
	}
	return nil
}

func (service *Service) authorizationCodeHandler(ctx context.Context) (auth.OAuthHandler, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.auth != nil {
		return service.auth, nil
	}
	stored, err := service.store.Load(ctx, service.key)
	if err != nil {
		return nil, sanitizeContext(ctx, ErrTokenStore)
	}
	var initial oauth2.TokenSource
	if stored != nil {
		initial = oauth2.StaticTokenSource(stored)
	}
	redirect := genericRedirectURL
	configuration := &auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{genericRedirectURL}, TokenEndpointAuthMethod: "none",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, ClientName: "datalon",
		}},
		RedirectURL: genericRedirectURL, Client: service.client, InitialTokenSource: initial,
		RequestRefreshToken: true,
	}
	if service.provider == ProviderSlack {
		redirect = slackRedirectURL
		configuration.DynamicClientRegistrationConfig = nil
		configuration.PreregisteredClient = &oauthex.ClientCredentials{ClientID: slackClientID}
		configuration.RedirectURL = redirect
	}
	configuration.AuthorizationCodeFetcher = func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		authorizationURL := args.URL
		if err := validateAuthorizationURL(authorizationURL, service.provider); err != nil {
			return nil, ErrInvalidResponse
		}
		if service.provider == ProviderSlack {
			if selector, ok := service.interaction.(WorkspaceSelector); ok {
				workspace, err := selector.SelectSlackWorkspace(ctx)
				if err != nil {
					return nil, sanitizeContext(ctx, ErrInteraction)
				}
				if workspace != "" {
					if !workspacePattern.MatchString(workspace) {
						return nil, errors.New("Slack workspace selection is invalid")
					}
					parsed, _ := url.Parse(authorizationURL)
					query := parsed.Query()
					query.Set("team", workspace)
					parsed.RawQuery = query.Encode()
					authorizationURL = parsed.String()
				}
			}
		}
		callback, err := service.interaction.Authorize(ctx, authorizationURL)
		if err != nil {
			return nil, sanitizeContext(ctx, ErrInteraction)
		}
		return parseCallback(callback, redirect, service.limits.maxCallbackBytes)
	}
	configuration.NewTokenSource = func(ctx context.Context, config *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
		return &savingSource{source: config.TokenSource(ctx, token), store: service.store, key: service.key}, nil
	}
	handler, err := auth.NewAuthorizationCodeHandler(configuration)
	if err != nil {
		panic("invalid static OAuth authorization-code policy")
	}
	service.auth = handler
	return handler, nil
}

func parseCallback(raw, expected string, maximum int) (*auth.AuthorizationResult, error) {
	if len(raw) == 0 || len(raw) > maximum {
		return nil, errors.New("OAuth callback is invalid")
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("OAuth callback is invalid")
	}
	wanted, _ := url.Parse(expected)
	if parsed.Scheme != wanted.Scheme || !strings.EqualFold(parsed.Host, wanted.Host) || parsed.Path != wanted.Path {
		return nil, errors.New("OAuth callback does not match the configured loopback redirect")
	}
	values := parsed.Query()
	if values.Get("error") != "" {
		return nil, ErrAuthorizationDenied
	}
	code, state, issuer := values.Get("code"), values.Get("state"), values.Get("iss")
	if code == "" || state == "" || len(code) > 8<<10 || len(state) > 8<<10 || len(issuer) > 8<<10 {
		return nil, errors.New("OAuth callback is invalid")
	}
	return &auth.AuthorizationResult{Code: code, State: state, Iss: issuer}, nil
}

func validateAuthorizationURL(raw string, provider Provider) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return ErrInvalidResponse
	}
	if provider == ProviderSlack {
		host := strings.ToLower(parsed.Hostname())
		if host != "slack.com" && !strings.HasSuffix(host, ".slack.com") {
			return ErrInvalidResponse
		}
	}
	return nil
}

func strictRemoteURL(raw string) (string, error) {
	if len(raw) == 0 || len(raw) > 16<<10 || strings.ContainsAny(raw, "\x00\r\n") {
		return "", errors.New("OAuth endpoint is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("OAuth endpoint must be a credential-free HTTPS URL")
	}
	return parsed.String(), nil
}

func (service *Service) requireInitialized() {
	if service == nil || service.client == nil || service.store == nil || service.interaction == nil || service.gate == nil {
		panic("initialized OAuth policy service is required")
	}
}

type boundedTransport struct {
	base    http.RoundTripper
	maximum int64
}

func (transport *boundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, errors.New("OAuth HTTP request failed")
	}
	if response == nil {
		return nil, ErrInvalidResponse
	}
	if response.Body == nil {
		return nil, ErrInvalidResponse
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, errors.New("OAuth HTTP redirects are disabled")
	}
	response.Body = &boundedBody{reader: io.LimitReader(response.Body, transport.maximum+1), closer: response.Body, remaining: transport.maximum}
	return response, nil
}

type boundedBody struct {
	reader    io.Reader
	closer    io.Closer
	remaining int64
}

func (body *boundedBody) Read(buffer []byte) (int, error) {
	if body.remaining <= 0 {
		var probe [1]byte
		if n, _ := body.reader.Read(probe[:]); n > 0 {
			return 0, errors.New("OAuth response exceeds configured limit")
		}
		return 0, io.EOF
	}
	if int64(len(buffer)) > body.remaining {
		buffer = buffer[:body.remaining]
	}
	n, err := body.reader.Read(buffer)
	body.remaining -= int64(n)
	return n, err
}

func (body *boundedBody) Close() error { return body.closer.Close() }

type safeStore struct {
	delegate mcp.TokenStore
	maximum  int
}

func (store *safeStore) Load(ctx context.Context, key string) (*oauth2.Token, error) {
	token, err := store.delegate.Load(ctx, key)
	if err != nil {
		return nil, sanitizeContext(ctx, ErrTokenStore)
	}
	if token != nil && !validToken(token, store.maximum) {
		return nil, ErrInvalidResponse
	}
	return token, nil
}

func (store *safeStore) Save(ctx context.Context, key string, token *oauth2.Token) error {
	if !validToken(token, store.maximum) {
		return ErrInvalidResponse
	}
	if err := store.delegate.Save(ctx, key, token); err != nil {
		return sanitizeContext(ctx, ErrTokenStore)
	}
	return nil
}

func validToken(token *oauth2.Token, maximum int) bool {
	return token != nil && token.AccessToken != "" && len(token.AccessToken) <= maximum &&
		len(token.RefreshToken) <= maximum && len(token.TokenType) <= 128 &&
		!strings.ContainsAny(token.AccessToken+token.RefreshToken+token.TokenType, "\x00\r\n")
}

type savingSource struct {
	source      oauth2.TokenSource
	store       mcp.TokenStore
	key         string
	mu          sync.Mutex
	fingerprint [sha256.Size]byte
	saved       bool
}

func (source *savingSource) Token() (*oauth2.Token, error) {
	token, err := source.source.Token()
	if err != nil {
		return nil, errors.New("OAuth token refresh failed")
	}
	fingerprint := sha256.Sum256([]byte(token.AccessToken + "\x00" + token.RefreshToken + "\x00" + token.Expiry.UTC().String()))
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.saved && source.fingerprint == fingerprint {
		return token, nil
	}
	if err := source.store.Save(context.Background(), source.key, token); err != nil {
		return nil, err
	}
	source.fingerprint, source.saved = fingerprint, true
	return token, nil
}

func sanitizeContext(ctx context.Context, fallback error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fallback
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	return (kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice) && reflect.ValueOf(value).IsNil()
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ auth.OAuthHandler = (*Service)(nil)
