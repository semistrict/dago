package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const (
	defaultRedirectURL = "http://127.0.0.1:53682/callback"
	maxTokenBytes      = 256 << 10
)

// Interaction obtains the redirect URL after presenting an authorization URL
// to a person. Implementations may use paste-back, a loopback listener, or an
// application-native browser flow.
type Interaction interface {
	Authorize(context.Context, string) (string, error)
}

// TokenStore persists OAuth tokens outside configuration files.
type TokenStore interface {
	Load(context.Context, string) (*oauth2.Token, error)
	Save(context.Context, string, *oauth2.Token) error
}

// FileTokenStore stores one mode-0600 token document per opaque key.
type FileTokenStore struct{ directory string }

// NewFileTokenStore creates a store without touching the filesystem.
func NewFileTokenStore(directory string) *FileTokenStore {
	if strings.TrimSpace(directory) == "" {
		panic("OAuth token directory is required")
	}
	return &FileTokenStore{directory: directory}
}

type tokenDocument struct {
	Version      int       `json:"version"`
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

// Load reads one bounded token. Missing tokens are represented by nil, nil.
func (store *FileTokenStore) Load(ctx context.Context, key string) (*oauth2.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := store.path(key)
	if err != nil {
		return nil, err
	}
	linkInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect OAuth token path: %w", err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("OAuth token is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open OAuth token: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect OAuth token: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("OAuth token is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTokenBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read OAuth token: %w", err)
	}
	if len(data) > maxTokenBytes {
		return nil, fmt.Errorf("OAuth token exceeds %d bytes", maxTokenBytes)
	}
	var document tokenDocument
	if err := json.Unmarshal(data, &document); err != nil || document.Version != 1 || document.AccessToken == "" {
		return nil, fmt.Errorf("OAuth token document is invalid")
	}
	return &oauth2.Token{AccessToken: document.AccessToken, TokenType: document.TokenType, RefreshToken: document.RefreshToken, Expiry: document.Expiry}, nil
}

// Save atomically replaces one token with private permissions.
func (store *FileTokenStore) Save(ctx context.Context, key string, token *oauth2.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if token == nil || token.AccessToken == "" {
		return fmt.Errorf("OAuth token is empty")
	}
	path, err := store.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(store.directory, 0o700); err != nil {
		return fmt.Errorf("prepare OAuth token directory: %w", err)
	}
	directoryInfo, err := os.Lstat(store.directory)
	if err != nil {
		return fmt.Errorf("inspect OAuth token directory: %w", err)
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return fmt.Errorf("OAuth token directory is not a directory")
	}
	if err := os.Chmod(store.directory, 0o700); err != nil {
		return fmt.Errorf("secure OAuth token directory: %w", err)
	}
	document := tokenDocument{Version: 1, AccessToken: token.AccessToken, TokenType: token.TokenType, RefreshToken: token.RefreshToken, Expiry: token.Expiry}
	encoded, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("encode OAuth token: %w", err)
	}
	temporary, err := os.CreateTemp(store.directory, ".oauth-token-*")
	if err != nil {
		return fmt.Errorf("create OAuth token: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure OAuth token: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write OAuth token: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync OAuth token: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close OAuth token: %w", err)
	}
	if err := replaceFile(temporaryPath, path); err != nil {
		return fmt.Errorf("replace OAuth token: %w", err)
	}
	committed = true
	return nil
}

func (store *FileTokenStore) path(key string) (string, error) {
	if store == nil || strings.TrimSpace(store.directory) == "" {
		panic("initialized OAuth token store is required")
	}
	if len(key) != 64 {
		return "", fmt.Errorf("OAuth token key is invalid")
	}
	if _, err := hex.DecodeString(key); err != nil {
		return "", fmt.Errorf("OAuth token key is invalid")
	}
	return filepath.Join(store.directory, key+".json"), nil
}

// OAuthHandler is a persistent authorization-code handler for one server.
type OAuthHandler struct {
	httpClient  *http.Client
	store       TokenStore
	interaction Interaction
	key         string
	mu          sync.Mutex
	delegate    *auth.AuthorizationCodeHandler
}

// NewOAuthHandler constructs a handler without network or filesystem I/O.
func NewOAuthHandler(httpClient *http.Client, store TokenStore, interaction Interaction, serverName, endpoint string) *OAuthHandler {
	if httpClient == nil {
		panic("OAuth HTTP client is required")
	}
	if nilOAuthDependency(store) {
		panic("OAuth token store is required")
	}
	if nilOAuthDependency(interaction) {
		panic("OAuth interaction is required")
	}
	if strings.TrimSpace(serverName) == "" {
		panic("OAuth server name is required")
	}
	if _, err := validateRemoteURL(endpoint); err != nil {
		panic(err)
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	digest := sha256.Sum256([]byte(serverName + "\x00" + endpoint))
	return &OAuthHandler{httpClient: &clientCopy, store: store, interaction: interaction, key: hex.EncodeToString(digest[:])}
}

// NewOAuthFactory binds shared OAuth dependencies for Client.
func NewOAuthFactory(httpClient *http.Client, store TokenStore, interaction Interaction) OAuthFactory {
	if httpClient == nil || nilOAuthDependency(store) || nilOAuthDependency(interaction) {
		panic("OAuth HTTP client, token store, and interaction are required")
	}
	return func(serverName string, server Server) (auth.OAuthHandler, error) {
		return NewOAuthHandler(httpClient, store, interaction, serverName, server.URL), nil
	}
}

func nilOAuthDependency(value any) bool {
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

func (handler *OAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if err := handler.ensureDelegate(ctx); err != nil {
		return nil, err
	}
	return handler.delegate.TokenSource(ctx)
}

func (handler *OAuthHandler) Authorize(ctx context.Context, request *http.Request, response *http.Response) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if err := handler.ensureDelegate(ctx); err != nil {
		return err
	}
	if err := handler.delegate.Authorize(ctx, request, response); err != nil {
		return fmt.Errorf("OAuth authorization failed: %w", err)
	}
	source, err := handler.delegate.TokenSource(ctx)
	if err != nil || source == nil {
		return fmt.Errorf("OAuth authorization returned no token")
	}
	_, err = source.Token()
	if err != nil {
		return fmt.Errorf("read OAuth token: %w", err)
	}
	return nil
}

func (handler *OAuthHandler) ensureDelegate(ctx context.Context) error {
	if handler == nil || handler.httpClient == nil || handler.store == nil || handler.interaction == nil {
		panic("initialized OAuth handler is required")
	}
	if handler.delegate != nil {
		return nil
	}
	stored, err := handler.store.Load(ctx, handler.key)
	if err != nil {
		return err
	}
	var initial oauth2.TokenSource
	if stored != nil {
		initial = oauth2.StaticTokenSource(stored)
	}
	delegate, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{defaultRedirectURL}, TokenEndpointAuthMethod: "none",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
			ClientName: "datalon",
		}},
		RedirectURL: defaultRedirectURL, Client: handler.httpClient, InitialTokenSource: initial,
		RequestRefreshToken: true,
		AuthorizationCodeFetcher: func(ctx context.Context, arguments *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			callback, err := handler.interaction.Authorize(ctx, arguments.URL)
			if err != nil {
				return nil, err
			}
			return parseAuthorizationCallback(callback)
		},
		NewTokenSource: func(ctx context.Context, config *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			return &savingTokenSource{source: config.TokenSource(ctx, token), store: handler.store, key: handler.key}, nil
		},
	})
	if err != nil {
		return fmt.Errorf("prepare OAuth authorization: %w", err)
	}
	handler.delegate = delegate
	return nil
}

type savingTokenSource struct {
	source      oauth2.TokenSource
	store       TokenStore
	key         string
	mu          sync.Mutex
	fingerprint [sha256.Size]byte
	hasSaved    bool
}

func (source *savingTokenSource) Token() (*oauth2.Token, error) {
	token, err := source.source.Token()
	if err != nil {
		return nil, err
	}
	encoded := token.AccessToken + "\x00" + token.TokenType + "\x00" + token.RefreshToken + "\x00" + token.Expiry.UTC().Format(time.RFC3339Nano)
	fingerprint := sha256.Sum256([]byte(encoded))
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.hasSaved && source.fingerprint == fingerprint {
		return token, nil
	}
	if err := source.store.Save(context.Background(), source.key, token); err != nil {
		return nil, err
	}
	source.fingerprint = fingerprint
	source.hasSaved = true
	return token, nil
}

func parseAuthorizationCallback(raw string) (*auth.AuthorizationResult, error) {
	if len(raw) > 16<<10 {
		return nil, fmt.Errorf("OAuth callback is too large")
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" {
		return nil, fmt.Errorf("OAuth callback URL is invalid")
	}
	if parsed.Scheme != "http" || !isLoopbackHost(parsed.Hostname()) || parsed.Path != "/callback" {
		return nil, fmt.Errorf("OAuth callback URL does not match the loopback redirect")
	}
	values := parsed.Query()
	if message := values.Get("error"); message != "" {
		return nil, fmt.Errorf("OAuth provider returned %s", message)
	}
	if values.Get("code") == "" || values.Get("state") == "" {
		return nil, fmt.Errorf("OAuth callback is missing code or state")
	}
	return &auth.AuthorizationResult{Code: values.Get("code"), State: values.Get("state"), Iss: values.Get("iss")}, nil
}

type oauthRoundTripper struct {
	base    http.RoundTripper
	handler auth.OAuthHandler
}

func (transport *oauthRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.roundTripAuthorized(request)
	if err != nil || (response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden) {
		return response, err
	}
	if err := transport.handler.Authorize(request.Context(), request, response); err != nil {
		return nil, err
	}
	retry := request.Clone(request.Context())
	if request.Body != nil {
		if request.GetBody == nil {
			return nil, fmt.Errorf("cannot retry OAuth request body")
		}
		retry.Body, err = request.GetBody()
		if err != nil {
			return nil, err
		}
	}
	return transport.roundTripAuthorized(retry)
}
func (transport *oauthRoundTripper) roundTripAuthorized(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	source, err := transport.handler.TokenSource(request.Context())
	if err != nil {
		return nil, err
	}
	if source != nil {
		token, err := source.Token()
		if err != nil {
			return nil, err
		}
		token.SetAuthHeader(clone)
	}
	return transport.base.RoundTrip(clone)
}

// Login connects one configured server and completes OAuth if challenged.
func Login(ctx context.Context, httpClient *http.Client, serverName string, server Server, store TokenStore, interaction Interaction) error {
	if httpClient == nil || store == nil || interaction == nil {
		panic("OAuth login dependencies are required")
	}
	if server.Auth != "oauth" {
		return fmt.Errorf("server %q is not configured for OAuth", serverName)
	}
	client := NewClient(httpClient, NewOAuthFactory(httpClient, store, interaction), Options{})
	bundle, err := client.Connect(ctx, Config{Servers: map[string]Server{serverName: server}})
	if err != nil {
		return err
	}
	defer bundle.Close()
	if len(bundle.Servers) != 1 || bundle.Servers[0].Error != "" {
		if len(bundle.Servers) == 1 {
			return fmt.Errorf("MCP login failed: %s", bundle.Servers[0].Error)
		}
		return fmt.Errorf("MCP login failed")
	}
	return nil
}

var _ auth.OAuthHandler = (*OAuthHandler)(nil)
