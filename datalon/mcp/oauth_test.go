package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

func TestFileTokenStoreRoundTripAndPrivatePermissions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := NewFileTokenStore(root)
	key := strings.Repeat("a", 64)
	want := &oauth2.Token{AccessToken: "access-secret", RefreshToken: "refresh-secret", TokenType: "Bearer", Expiry: time.Unix(1_800_000_000, 0).UTC()}
	if err := store.Save(t.Context(), key, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || !got.Expiry.Equal(want.Expiry) {
		t.Fatalf("token = %#v", got)
	}
	info, err := os.Stat(store.directory + "/" + key + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestOAuthHandlerFactoryRejectsTypedNilResult(t *testing.T) {
	var handler *fakeOAuthHandler
	client := &Client{oauth: func(string, Server) (sdkauth.OAuthHandler, error) { return handler, nil }}
	if _, err := client.oauthHandler("remote", Server{Auth: "oauth"}); err == nil || !strings.Contains(err.Error(), "no handler") {
		t.Fatalf("typed-nil OAuth handler error = %v", err)
	}
}

func TestNilOAuthDependencyCoversEveryNilableKind(t *testing.T) {
	var pointer *int
	var mapping map[string]int
	var function func()
	var slice []int
	var channel chan int
	var interfaceValue any
	for name, value := range map[string]any{
		"pointer": pointer, "map": mapping, "function": function,
		"slice": slice, "channel": channel, "interface": interfaceValue,
	} {
		t.Run(name, func(t *testing.T) {
			if !nilOAuthDependency(value) {
				t.Fatal("typed nil was not detected")
			}
		})
	}
}

func TestParseAuthorizationCallbackConfinesRedirect(t *testing.T) {
	t.Parallel()
	result, err := parseAuthorizationCallback("http://127.0.0.1:53682/callback?code=abc&state=xyz&iss=issuer")
	if err != nil {
		t.Fatal(err)
	}
	if result.Code != "abc" || result.State != "xyz" || result.Iss != "issuer" {
		t.Fatalf("result = %#v", result)
	}
	for _, raw := range []string{"https://example.test/callback?code=a&state=b", "http://127.0.0.1:53682/other?code=a&state=b", "http://127.0.0.1:53682/callback?code=a", "http://127.0.0.1:53682/callback?error=denied"} {
		if _, err := parseAuthorizationCallback(raw); err == nil {
			t.Errorf("callback %q succeeded", raw)
		}
	}
}

func TestOAuthRoundTripperRetriesOnceWithScopedBearer(t *testing.T) {
	t.Parallel()
	handler := &fakeOAuthHandler{tokens: []*oauth2.Token{{AccessToken: "old"}, {AccessToken: "new"}}}
	var mu sync.Mutex
	var authorizations []string
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		count := len(authorizations)
		mu.Unlock()
		status := http.StatusOK
		if count == 1 {
			status = http.StatusUnauthorized
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
	transport := &oauthRoundTripper{base: base, handler: handler}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.test/mcp", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || handler.authorized != 1 {
		t.Fatalf("status = %d, authorized = %d", response.StatusCode, handler.authorized)
	}
	if len(authorizations) != 2 || authorizations[0] != "Bearer old" || authorizations[1] != "Bearer new" {
		t.Fatalf("headers = %v", authorizations)
	}
}

func TestOAuthHandlerCompletesDynamicRegistrationAndPersistsToken(t *testing.T) {
	t.Parallel()
	var authorizationServer *httptest.Server
	mux := http.NewServeMux()
	authorizationServer = httptest.NewServer(mux)
	defer authorizationServer.Close()
	endpoint := authorizationServer.URL + "/mcp"
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(t, writer, map[string]any{"resource": endpoint, "authorization_servers": []string{authorizationServer.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(t, writer, map[string]any{
			"issuer": authorizationServer.URL, "authorization_endpoint": authorizationServer.URL + "/authorize",
			"token_endpoint": authorizationServer.URL + "/token", "registration_endpoint": authorizationServer.URL + "/register",
			"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("registration method = %s", request.Method)
		}
		writer.WriteHeader(http.StatusCreated)
		writeJSON(t, writer, map[string]any{
			"client_id": "dynamic-client", "redirect_uris": []string{defaultRedirectURL}, "token_endpoint_auth_method": "none",
			"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		})
	})
	mux.HandleFunc("/token", func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Error(err)
		}
		if request.Form.Get("code") != "authorization-code" {
			t.Errorf("code = %q", request.Form.Get("code"))
		}
		writeJSON(t, writer, map[string]any{"access_token": "persisted-access", "refresh_token": "persisted-refresh", "token_type": "Bearer", "expires_in": 3600})
	})
	interaction := interactionFunc(func(_ context.Context, authorizationURL string) (string, error) {
		parsed, err := url.Parse(authorizationURL)
		if err != nil {
			return "", err
		}
		if parsed.Path != "/authorize" || parsed.Query().Get("code_challenge") == "" {
			return "", fmt.Errorf("invalid authorization URL")
		}
		return defaultRedirectURL + "?code=authorization-code&state=" + url.QueryEscape(parsed.Query().Get("state")), nil
	})
	store := NewFileTokenStore(t.TempDir())
	handler := NewOAuthHandler(authorizationServer.Client(), store, interaction, "remote", endpoint)
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	response := &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: http.NoBody, Request: request}
	response.Header.Set("WWW-Authenticate", `Bearer resource_metadata="`+authorizationServer.URL+`/.well-known/oauth-protected-resource/mcp"`)
	if err := handler.Authorize(t.Context(), request, response); err != nil {
		t.Fatal(err)
	}
	source, err := handler.TokenSource(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "persisted-access" || token.RefreshToken != "persisted-refresh" {
		t.Fatalf("token = %#v", token)
	}
	loaded, err := store.Load(t.Context(), handler.key)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccessToken != "persisted-access" || loaded.RefreshToken != "persisted-refresh" {
		t.Fatalf("stored = %#v", loaded)
	}
}

func TestOAuthConstructorsRejectTypedNilDependencies(t *testing.T) {
	var store *FileTokenStore
	var interaction interactionFunc
	for name, call := range map[string]func(){
		"handler store": func() {
			NewOAuthHandler(http.DefaultClient, store, interactionFunc(func(context.Context, string) (string, error) { return "", nil }), "server", "https://example.test/mcp")
		},
		"handler interaction": func() {
			NewOAuthHandler(http.DefaultClient, NewFileTokenStore(t.TempDir()), interaction, "server", "https://example.test/mcp")
		},
		"factory store": func() {
			NewOAuthFactory(http.DefaultClient, store, interactionFunc(func(context.Context, string) (string, error) { return "", nil }))
		},
		"factory interaction": func() { NewOAuthFactory(http.DefaultClient, NewFileTokenStore(t.TempDir()), interaction) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("typed nil dependency did not panic")
				}
			}()
			call()
		})
	}
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Error(err)
	}
}

type interactionFunc func(context.Context, string) (string, error)

func (function interactionFunc) Authorize(ctx context.Context, rawURL string) (string, error) {
	return function(ctx, rawURL)
}

type fakeOAuthHandler struct {
	mu                sync.Mutex
	tokens            []*oauth2.Token
	index, authorized int
}

func (handler *fakeOAuthHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	index := handler.index
	if index >= len(handler.tokens) {
		index = len(handler.tokens) - 1
	}
	return oauth2.StaticTokenSource(handler.tokens[index]), nil
}
func (handler *fakeOAuthHandler) Authorize(_ context.Context, _ *http.Request, response *http.Response) error {
	response.Body.Close()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.authorized++
	if handler.index+1 < len(handler.tokens) {
		handler.index++
	}
	return nil
}
