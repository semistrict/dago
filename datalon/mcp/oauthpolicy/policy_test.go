package oauthpolicy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/dago/datalon/mcp"
	"golang.org/x/oauth2"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type memoryStore struct {
	mu    sync.Mutex
	token *oauth2.Token
	key   string
	err   error
}

func (store *memoryStore) Load(ctx context.Context, key string) (*oauth2.Token, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.token, store.err
}

func (store *memoryStore) Save(ctx context.Context, key string, token *oauth2.Token) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.key, store.token = key, token
	return store.err
}

type interaction struct {
	authorizationURL string
	workspace        string
	device           DeviceCode
	authorize        func(string) string
	presentErr       error
}

func (item *interaction) Authorize(ctx context.Context, authorizationURL string) (string, error) {
	item.authorizationURL = authorizationURL
	if item.authorize == nil {
		return "", errors.New("unexpected authorization-code flow")
	}
	return item.authorize(authorizationURL), nil
}

func (item *interaction) SelectSlackWorkspace(context.Context) (string, error) {
	return item.workspace, nil
}

func (item *interaction) PresentDeviceCode(ctx context.Context, device DeviceCode) error {
	item.device = device
	return item.presentErr
}

func TestSelectUsesExactProviderHosts(t *testing.T) {
	t.Parallel()
	tests := map[string]Provider{
		"https://slack.com/mcp":                  ProviderSlack,
		"https://mcp.slack.com/v1":               ProviderSlack,
		"https://api.githubcopilot.com/mcp/":     ProviderGitHub,
		"https://slack.com.attacker.example/mcp": ProviderGeneric,
		"https://github.com/mcp":                 ProviderGeneric,
		"not a url":                              ProviderGeneric,
	}
	for endpoint, expected := range tests {
		if actual := Select(endpoint); actual != expected {
			t.Fatalf("Select(%q) = %q, want %q", endpoint, actual, expected)
		}
	}
}

func TestNewIsNoIOAndRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("network must not be used")
	})}
	service := New(client, &memoryStore{}, &interaction{}, "server", mcp.Server{URL: "https://example.com/mcp", Auth: "oauth"}, Options{})
	if service.Provider() != ProviderGeneric || calls != 0 {
		t.Fatalf("constructor performed I/O or selected wrong provider: provider=%q calls=%d", service.Provider(), calls)
	}
	for _, server := range []mcp.Server{
		{URL: "http://example.com/mcp", Auth: "oauth"},
		{URL: "https://name:" + "secret@example.com/mcp", Auth: "oauth"},
		{URL: "https://example.com/mcp#fragment", Auth: "oauth"},
		{URL: "https://example.com/mcp"},
	} {
		server := server
		t.Run(server.URL+server.Auth, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New did not panic for invalid static configuration")
				}
			}()
			New(client, &memoryStore{}, &interaction{}, "server", server, Options{})
		})
	}
	var typedNilStore *memoryStore
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a typed nil token store")
		}
	}()
	New(client, typedNilStore, &interaction{}, "server", mcp.Server{URL: "https://example.com/mcp", Auth: "oauth"}, Options{})
}

func TestOptionsRejectUnboundedConfiguration(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted an effectively unbounded response limit")
		}
	}()
	New(&http.Client{}, &memoryStore{}, &interaction{}, "server", mcp.Server{URL: "https://example.com/mcp", Auth: "oauth"}, Options{MaxResponseBytes: maximumBodyBytes + 1})
}

func TestSlackAuthorizationUsesPKCEStateWorkspaceAndExactCallback(t *testing.T) {
	t.Parallel()
	store := &memoryStore{}
	ui := &interaction{workspace: "T012WORKSPACE"}
	ui.authorize = func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		if query.Get("team") != ui.workspace || query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
			t.Fatalf("authorization URL lacks workspace or PKCE: %s", raw)
		}
		return slackRedirectURL + "?code=one-time-code&state=" + url.QueryEscape(query.Get("state"))
	}
	client := &http.Client{Transport: slackTransport(t)}
	service := New(client, store, ui, "slack", mcp.Server{URL: "https://mcp.slack.com/mcp", Auth: "oauth"}, Options{})
	request, _ := http.NewRequest(http.MethodPost, "https://mcp.slack.com/mcp", nil)
	response := jsonResponse(http.StatusUnauthorized, `{}`)
	response.Header.Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.slack.com/.well-known/oauth-protected-resource"`)
	if err := service.Authorize(context.Background(), request, response); err != nil {
		t.Fatal(err)
	}
	if store.token == nil || store.token.AccessToken != "stored-access" || len(store.key) != 64 {
		t.Fatal("successful Slack authorization did not persist its token under an opaque key")
	}
}

func TestSlackAuthorizationRejectsWrongStateAndWorkspace(t *testing.T) {
	t.Parallel()
	ui := &interaction{workspace: "T012WORKSPACE"}
	ui.authorize = func(string) string {
		return slackRedirectURL + "?code=one-time-code&state=attacker-state"
	}
	service := New(&http.Client{Transport: slackTransport(t)}, &memoryStore{}, ui, "slack", mcp.Server{URL: "https://mcp.slack.com/mcp", Auth: "oauth"}, Options{})
	request, _ := http.NewRequest(http.MethodPost, "https://mcp.slack.com/mcp", nil)
	response := jsonResponse(http.StatusUnauthorized, `{}`)
	response.Header.Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.slack.com/.well-known/oauth-protected-resource"`)
	if err := service.Authorize(context.Background(), request, response); err == nil {
		t.Fatal("authorization accepted the wrong callback state")
	}

	ui = &interaction{workspace: "bad workspace?"}
	ui.authorize = func(string) string { t.Fatal("invalid workspace reached browser interaction"); return "" }
	service = New(&http.Client{Transport: slackTransport(t)}, &memoryStore{}, ui, "slack", mcp.Server{URL: "https://mcp.slack.com/mcp", Auth: "oauth"}, Options{})
	response = jsonResponse(http.StatusUnauthorized, `{}`)
	response.Header.Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.slack.com/.well-known/oauth-protected-resource"`)
	if err := service.Authorize(context.Background(), request, response); err == nil {
		t.Fatal("authorization accepted an unsafe workspace selection")
	}
}

func TestCallbackRequiresExactLoopbackAndDoesNotEchoProviderError(t *testing.T) {
	t.Parallel()
	for _, callback := range []string{
		"http://localhost:9999/callback?code=x&state=y",
		"http://127.0.0.1:3118/callback?code=x&state=y",
		"http://localhost:3118/other?code=x&state=y",
	} {
		if _, err := parseCallback(callback, slackRedirectURL, 16<<10); err == nil {
			t.Fatalf("accepted mismatched callback %q", callback)
		}
	}
	secret := "provider-detail-containing-secret"
	_, err := parseCallback(slackRedirectURL+"?error="+secret+"&state=x", slackRedirectURL, 16<<10)
	if !errors.Is(err, ErrAuthorizationDenied) || strings.Contains(err.Error(), secret) {
		t.Fatalf("provider error was not safely reduced: %v", err)
	}
}

func TestGitHubDeviceFlowIsBoundedAndPersistsToken(t *testing.T) {
	t.Parallel()
	var requests []url.Values
	responses := []string{
		`{"device_code":"device-secret","user_code":"ABCD-EFGH","verification_uri":"https://github.com/login/device","expires_in":600,"interval":0}`,
		`{"error":"authorization_pending"}`,
		`{"access_token":"github-access","token_type":"bearer","scope":"repo"}`,
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		values, _ := url.ParseQuery(string(body))
		requests = append(requests, values)
		if len(responses) == 0 {
			t.Fatal("device flow exceeded expected request count")
		}
		bodyText := responses[0]
		responses = responses[1:]
		return jsonResponse(http.StatusOK, bodyText), nil
	})}
	store, ui := &memoryStore{}, &interaction{}
	service := New(client, store, ui, "github", mcp.Server{URL: "https://api.githubcopilot.com/mcp", Auth: "oauth"}, Options{MinPollInterval: time.Nanosecond})
	service.wait = func(ctx context.Context, delay time.Duration) error { return ctx.Err() }
	if err := service.Authorize(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if ui.device.UserCode != "ABCD-EFGH" || ui.device.VerificationURI != "https://github.com/login/device" {
		t.Fatalf("wrong public device instructions: %#v", ui.device)
	}
	if store.token == nil || store.token.AccessToken != "github-access" {
		t.Fatal("GitHub device flow did not persist the token")
	}
	if len(requests) != 3 || requests[0].Get("client_id") != githubClientID || requests[1].Get("device_code") != "device-secret" {
		t.Fatalf("wrong device-flow requests: %#v", requests)
	}
}

func TestGitHubDeviceFlowHonorsCancellationAndPollLimit(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() == githubDeviceURL {
			return jsonResponse(http.StatusOK, `{"device_code":"secret","user_code":"ABCD","verification_uri":"https://github.com/login/device","expires_in":600,"interval":0}`), nil
		}
		return jsonResponse(http.StatusOK, `{"error":"authorization_pending"}`), nil
	})}
	service := New(client, &memoryStore{}, &interaction{}, "github", mcp.Server{URL: "https://api.githubcopilot.com/mcp", Auth: "oauth"}, Options{MaxPolls: 2, MinPollInterval: time.Nanosecond})
	service.wait = func(ctx context.Context, delay time.Duration) error { return ctx.Err() }
	if err := service.Authorize(context.Background(), nil, nil); !errors.Is(err, ErrDeviceCode) {
		t.Fatalf("poll limit returned %v, want ErrDeviceCode", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Authorize(cancelled, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation returned %v", err)
	}
}

func TestGitHubRejectsUntrustedVerificationURI(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"device_code":"secret","user_code":"ABCD","verification_uri":"https://github.com.attacker.example/login/device","expires_in":600}`), nil
	})}
	ui := &interaction{}
	service := New(client, &memoryStore{}, ui, "github", mcp.Server{URL: "https://api.githubcopilot.com/mcp", Auth: "oauth"}, Options{})
	if err := service.Authorize(context.Background(), nil, nil); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("untrusted verification URI returned %v", err)
	}
	if ui.device.UserCode != "" {
		t.Fatal("untrusted verification URI reached the interaction")
	}
}

func TestProviderSecretsNeverAppearInErrors(t *testing.T) {
	t.Parallel()
	secret := "sensitive-device-or-token-value"
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(secret)
	})}
	service := New(client, &memoryStore{err: errors.New(secret)}, &interaction{}, "github", mcp.Server{URL: "https://api.githubcopilot.com/mcp", Auth: "oauth"}, Options{})
	_, err := service.TokenSource(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("token-store error exposed a secret: %v", err)
	}
	service = New(client, &memoryStore{}, &interaction{}, "github", mcp.Server{URL: "https://api.githubcopilot.com/mcp", Auth: "oauth"}, Options{})
	err = service.Authorize(context.Background(), nil, nil)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("transport error exposed a secret: %v", err)
	}
}

func TestBoundedTransportRejectsOversizedResponse(t *testing.T) {
	t.Parallel()
	transport := &boundedTransport{maximum: 4, base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, "12345"), nil
	})}
	request, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	if err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func slackTransport(t *testing.T) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case "https://mcp.slack.com/.well-known/oauth-protected-resource":
			return jsonResponse(http.StatusOK, `{"resource":"https://mcp.slack.com/mcp","authorization_servers":["https://slack.com"]}`), nil
		case "https://slack.com/.well-known/oauth-authorization-server":
			return jsonResponse(http.StatusOK, `{"issuer":"https://slack.com","authorization_endpoint":"https://slack.com/oauth/v2/authorize","token_endpoint":"https://slack.com/api/oauth.v2.access","response_types_supported":["code"],"grant_types_supported":["authorization_code","refresh_token"],"token_endpoint_auth_methods_supported":["none"],"code_challenge_methods_supported":["S256"]}`), nil
		case "https://slack.com/api/oauth.v2.access":
			body, _ := io.ReadAll(request.Body)
			values, _ := url.ParseQuery(string(body))
			clientID, _, hasBasic := request.BasicAuth()
			if values.Get("client_id") != slackClientID && (!hasBasic || clientID != slackClientID) {
				t.Fatalf("Slack public client ID was omitted: headers=%v body=%s", request.Header, body)
			}
			if values.Get("code_verifier") == "" || values.Get("code") != "one-time-code" {
				t.Fatalf("invalid Slack token exchange: %s", body)
			}
			return jsonResponse(http.StatusOK, `{"access_token":"stored-access","token_type":"bearer"}`), nil
		default:
			t.Fatalf("unexpected OAuth request: %s %s", request.Method, request.URL)
			return nil, errors.New("unexpected request")
		}
	})
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}
