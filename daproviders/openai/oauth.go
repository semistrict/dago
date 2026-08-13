//go:build !tinygo

package openai

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	DefaultOAuthIssuer   = "https://auth.openai.com"
	DefaultOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	defaultOAuthPort     = 1455
	fallbackOAuthPort    = 1457
)

// OAuthTokens is the language-neutral credential record persisted by OAuthSession.
// The file is private to this library and is never discovered from another app.
type OAuthTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token,omitempty"`
	AccountID    string    `json:"account_id,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
}

// OAuthOptions configures an authorization-code flow with PKCE.
type OAuthOptions struct {
	ClientID   string
	Issuer     string
	Port       int
	HTTPClient *http.Client
	StorePath  string
	// OpenURL receives the authorization URL. A CLI can open a browser; a server
	// can print or otherwise deliver it. Login does not launch processes itself.
	OpenURL func(string) error
	// Listener is primarily useful for embedding and deterministic tests. Its
	// address must be registered with the authorization server.
	Listener net.Listener
}

// OAuthSession refreshes and optionally persists a caller-owned OAuth token set.
type OAuthSession struct {
	mu      sync.Mutex
	options OAuthOptions
	tokens  OAuthTokens
}

// Login performs a local-loopback authorization-code flow with PKCE and returns
// a refreshable session. It validates both callback state and PKCE exchange data.
func Login(ctx context.Context, options OAuthOptions) (*OAuthSession, error) {
	options = oauthDefaults(options)
	listener := options.Listener
	var err error
	if listener == nil {
		listener, err = listenOAuth(options.Port)
		if err != nil {
			return nil, err
		}
	}
	defer listener.Close()
	port, err := listenerPort(listener)
	if err != nil {
		return nil, err
	}
	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)
	state, err := randomURLToken(32)
	if err != nil {
		return nil, fmt.Errorf("openai: generate OAuth state: %w", err)
	}
	verifier, err := randomURLToken(64)
	if err != nil {
		return nil, fmt.Errorf("openai: generate PKCE verifier: %w", err)
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	authorizeURL, err := buildAuthorizeURL(options, redirectURI, state, challenge)
	if err != nil {
		return nil, err
	}

	type callback struct {
		code string
		err  error
	}
	callbackResult := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(writer http.ResponseWriter, request *http.Request) {
		if subtle.ConstantTimeCompare([]byte(request.URL.Query().Get("state")), []byte(state)) != 1 {
			http.Error(writer, "Invalid authorization state.", http.StatusBadRequest)
			select {
			case callbackResult <- callback{err: fmt.Errorf("openai: OAuth callback state mismatch")}:
			default:
			}
			return
		}
		if code := request.URL.Query().Get("error"); code != "" {
			description := request.URL.Query().Get("error_description")
			http.Error(writer, "Authorization failed. You may close this window.", http.StatusBadRequest)
			select {
			case callbackResult <- callback{err: fmt.Errorf("openai: OAuth callback %s: %s", code, description)}:
			default:
			}
			return
		}
		code := request.URL.Query().Get("code")
		if code == "" {
			http.Error(writer, "Missing authorization code.", http.StatusBadRequest)
			select {
			case callbackResult <- callback{err: fmt.Errorf("openai: OAuth callback omitted authorization code")}:
			default:
			}
			return
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(writer, "<!doctype html><title>Authorization complete</title><p>Authorization complete. You may close this window.</p>")
		select {
		case callbackResult <- callback{code: code}:
		default:
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
	}()

	if options.OpenURL == nil {
		_ = server.Shutdown(context.Background())
		return nil, fmt.Errorf("openai: OAuth OpenURL callback is required; authorization URL: %s", authorizeURL)
	}
	if err := options.OpenURL(authorizeURL); err != nil {
		_ = server.Shutdown(context.Background())
		return nil, fmt.Errorf("openai: deliver authorization URL: %w", err)
	}

	var result callback
	select {
	case <-ctx.Done():
		_ = server.Shutdown(context.Background())
		return nil, ctx.Err()
	case err := <-serveDone:
		if err != nil {
			return nil, fmt.Errorf("openai: OAuth callback server: %w", err)
		}
		return nil, fmt.Errorf("openai: OAuth callback server stopped before authorization")
	case result = <-callbackResult:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	if result.err != nil {
		return nil, result.err
	}
	tokens, err := exchangeAuthorizationCode(ctx, options, redirectURI, verifier, result.code)
	if err != nil {
		return nil, err
	}
	session := &OAuthSession{options: options, tokens: tokens}
	if err := session.persist(); err != nil {
		return nil, err
	}
	return session, nil
}

// LoadOAuthSession opens a token file previously written by this package.
func LoadOAuthSession(path string, options OAuthOptions) (*OAuthSession, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("openai: OAuth token path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("openai: read OAuth tokens: %w", err)
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("openai: OAuth token file is too large")
	}
	var tokens OAuthTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, fmt.Errorf("openai: decode OAuth tokens: %w", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return nil, fmt.Errorf("openai: OAuth token file is incomplete")
	}
	options.StorePath = path
	options = oauthDefaults(options)
	return &OAuthSession{options: options, tokens: tokens}, nil
}

func (session *OAuthSession) Credentials(ctx context.Context) (Credentials, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.tokens.AccessToken == "" {
		return Credentials{}, fmt.Errorf("openai: OAuth session has no access token")
	}
	if session.tokens.ExpiresAt.IsZero() || time.Until(session.tokens.ExpiresAt) > time.Minute {
		return Credentials{AccessToken: session.tokens.AccessToken, AccountID: session.tokens.AccountID}, nil
	}
	if session.tokens.RefreshToken == "" {
		return Credentials{}, fmt.Errorf("openai: OAuth access token expired and no refresh token is available")
	}
	tokens, err := refreshOAuthTokens(ctx, session.options, session.tokens)
	if err != nil {
		return Credentials{}, err
	}
	session.tokens = tokens
	if err := session.persist(); err != nil {
		return Credentials{}, err
	}
	return Credentials{AccessToken: tokens.AccessToken, AccountID: tokens.AccountID}, nil
}

// Tokens returns a copy suitable for displaying non-secret metadata. Callers must
// treat AccessToken, RefreshToken, and IDToken as secrets.
func (session *OAuthSession) Tokens() OAuthTokens {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.tokens
}

func oauthDefaults(options OAuthOptions) OAuthOptions {
	if options.ClientID == "" {
		options.ClientID = DefaultOAuthClientID
	}
	if options.Issuer == "" {
		options.Issuer = DefaultOAuthIssuer
	}
	options.Issuer = strings.TrimRight(options.Issuer, "/")
	if options.Port == 0 {
		options.Port = defaultOAuthPort
	}
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	return options
}

func listenOAuth(port int) (net.Listener, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil || port != defaultOAuthPort {
		if err != nil {
			return nil, fmt.Errorf("openai: listen for OAuth callback: %w", err)
		}
		return listener, nil
	}
	listener, fallbackErr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", fallbackOAuthPort))
	if fallbackErr != nil {
		return nil, fmt.Errorf("openai: OAuth callback ports %d and %d are unavailable: %v; %v", defaultOAuthPort, fallbackOAuthPort, err, fallbackErr)
	}
	return listener, nil
}

func listenerPort(listener net.Listener) (int, error) {
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.Port <= 0 {
		return 0, fmt.Errorf("openai: OAuth listener must use TCP")
	}
	return address.Port, nil
}

func buildAuthorizeURL(options OAuthOptions, redirectURI, state, challenge string) (string, error) {
	endpoint, err := url.Parse(options.Issuer + "/oauth/authorize")
	if err != nil {
		return "", fmt.Errorf("openai: OAuth issuer: %w", err)
	}
	query := endpoint.Query()
	query.Set("response_type", "code")
	query.Set("client_id", options.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("scope", "openid profile email offline_access api.connectors.read api.connectors.invoke")
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("id_token_add_organizations", "true")
	query.Set("state", state)
	query.Set("originator", "dago")
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

func exchangeAuthorizationCode(ctx context.Context, options OAuthOptions, redirectURI, verifier, code string) (OAuthTokens, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"client_id": {options.ClientID}, "code_verifier": {verifier},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, options.Issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return OAuthTokens{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := doOAuthJSON(options.HTTPClient, request, &response); err != nil {
		return OAuthTokens{}, fmt.Errorf("openai: exchange OAuth code: %w", err)
	}
	if response.AccessToken == "" || response.RefreshToken == "" {
		return OAuthTokens{}, fmt.Errorf("openai: OAuth token response is incomplete")
	}
	result := OAuthTokens{AccessToken: response.AccessToken, RefreshToken: response.RefreshToken, IDToken: response.IDToken}
	result.AccountID, _ = accountIDFromJWT(response.IDToken)
	result.ExpiresAt = tokenExpiry(response.AccessToken, response.ExpiresIn)
	return result, nil
}

func refreshOAuthTokens(ctx context.Context, options OAuthOptions, previous OAuthTokens) (OAuthTokens, error) {
	payload, err := json.Marshal(map[string]string{
		"client_id": options.ClientID, "grant_type": "refresh_token", "refresh_token": previous.RefreshToken,
	})
	if err != nil {
		return OAuthTokens{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, options.Issuer+"/oauth/token", strings.NewReader(string(payload)))
	if err != nil {
		return OAuthTokens{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := doOAuthJSON(options.HTTPClient, request, &response); err != nil {
		return OAuthTokens{}, fmt.Errorf("openai: refresh OAuth token: %w", err)
	}
	if response.AccessToken == "" {
		return OAuthTokens{}, fmt.Errorf("openai: refresh response omitted access token")
	}
	result := previous
	result.AccessToken = response.AccessToken
	if response.RefreshToken != "" {
		result.RefreshToken = response.RefreshToken
	}
	if response.IDToken != "" {
		result.IDToken = response.IDToken
		result.AccountID, _ = accountIDFromJWT(response.IDToken)
	}
	result.ExpiresAt = tokenExpiry(response.AccessToken, response.ExpiresIn)
	return result, nil
}

func doOAuthJSON(client *http.Client, request *http.Request, output any) error {
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var body struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(data, &body)
		message := body.ErrorDescription
		if message == "" {
			message = body.Error
		}
		if message == "" {
			message = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("token endpoint returned status %d: %s", response.StatusCode, message)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}
	return nil
}

func tokenExpiry(accessToken string, expiresIn int64) time.Time {
	if expiresIn > 0 {
		return time.Now().Add(time.Duration(expiresIn) * time.Second)
	}
	if claims, err := jwtClaims(accessToken); err == nil {
		if raw, ok := claims["exp"].(float64); ok && raw > 0 {
			return time.Unix(int64(raw), 0)
		}
	}
	return time.Time{}
}

func accountIDFromJWT(idToken string) (string, error) {
	claims, err := jwtClaims(idToken)
	if err != nil {
		return "", err
	}
	value, _ := claims["chatgpt_account_id"].(string)
	return value, nil
}

// jwtClaims only extracts routing metadata from a token already received over the
// configured TLS token endpoint. It does not authenticate arbitrary JWTs.
func jwtClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT claims: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("decode JWT claims: %w", err)
	}
	return claims, nil
}

func randomURLToken(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (session *OAuthSession) persist() error {
	if session.options.StorePath == "" {
		return nil
	}
	data, err := json.MarshalIndent(session.tokens, "", "  ")
	if err != nil {
		return fmt.Errorf("openai: encode OAuth tokens: %w", err)
	}
	path := filepath.Clean(session.options.StorePath)
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("openai: create OAuth token directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".oauth-*")
	if err != nil {
		return fmt.Errorf("openai: create OAuth token file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		cleanup()
		return fmt.Errorf("openai: persist OAuth tokens: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("openai: secure OAuth token file: %w", err)
	}
	return nil
}
