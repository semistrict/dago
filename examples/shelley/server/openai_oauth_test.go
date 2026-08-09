package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	dopenai "github.com/semistrict/dago/providers/openai"
)

type staticOAuthCredentials struct{}

func (staticOAuthCredentials) Credentials(context.Context) (dopenai.Credentials, error) {
	return dopenai.Credentials{AccessToken: "access", AccountID: "account"}, nil
}

func oauthTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestOpenAIOAuthLoadsPersistedSessionAndBuildsLuna(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "openai-oauth.json")
	if err := os.WriteFile(storePath, []byte(`{"access_token":"access","refresh_token":"refresh","account_id":"account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	controller := NewOpenAIOAuth(storePath, oauthTestLogger())
	status := controller.Status()
	if status.State != "complete" || !status.Ready || status.ModelID != OpenAISubscriptionModelID {
		t.Fatalf("Status() = %#v", status)
	}
	built, err := controller.BuiltModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 || built[0].ID != OpenAISubscriptionModelID || built[0].Source != "OpenAI subscription" {
		t.Fatalf("BuiltModels() = %#v", built)
	}
	profile := built[0].Chat.Profile()
	if profile.Model != OpenAISubscriptionModelID || profile.Provider != "openai" || !profile.ToolCalling {
		t.Fatalf("Dago profile = %#v", profile)
	}
	if !profile.SupportsImages || !profile.SupportsReasoning || profile.ContextWindow <= 0 {
		t.Fatalf("chat capabilities = %#v", profile)
	}
}

func TestOpenAIOAuthStartCompletesOnlyAfterCatalogRefresh(t *testing.T) {
	controller := NewOpenAIOAuth(filepath.Join(t.TempDir(), "openai-oauth.json"), oauthTestLogger())
	releaseLogin := make(chan struct{})
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	controller.login = func(ctx context.Context, options dopenai.OAuthOptions) (dopenai.CredentialSource, error) {
		if err := options.OpenURL("https://auth.example/authorize"); err != nil {
			return nil, err
		}
		select {
		case <-releaseLogin:
			return staticOAuthCredentials{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	controller.SetOnChange(func(context.Context) error {
		close(refreshStarted)
		<-releaseRefresh
		return nil
	})

	authorizationURL, err := controller.Start()
	if err != nil {
		t.Fatal(err)
	}
	if authorizationURL != "https://auth.example/authorize" || controller.Status().State != "pending" {
		t.Fatalf("Start() = %q, status %#v", authorizationURL, controller.Status())
	}
	if _, err := controller.Start(); err == nil {
		t.Fatal("second Start() succeeded while login was pending")
	}
	close(releaseLogin)
	<-refreshStarted
	if status := controller.Status(); status.State != "pending" || status.Ready {
		t.Fatalf("status became complete before refresh: %#v", status)
	}
	close(releaseRefresh)
	waitForOAuthState(t, controller, "complete")
	if !controller.Status().Ready {
		t.Fatalf("completed status = %#v", controller.Status())
	}
}

func TestOpenAIOAuthClearCancelsPendingLoginAndRemovesCredentials(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "openai-oauth.json")
	if err := os.WriteFile(storePath, []byte("credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller := NewOpenAIOAuth(storePath, oauthTestLogger())
	controller.session = staticOAuthCredentials{}
	controller.state = "complete"
	loginCanceled := make(chan struct{})
	controller.login = func(ctx context.Context, options dopenai.OAuthOptions) (dopenai.CredentialSource, error) {
		if err := options.OpenURL("https://auth.example/authorize"); err != nil {
			return nil, err
		}
		<-ctx.Done()
		close(loginCanceled)
		return nil, ctx.Err()
	}
	if _, err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan struct{})
	controller.SetOnChange(func(context.Context) error { close(refreshed); return nil })
	if err := controller.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-loginCanceled
	<-refreshed
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("credential file still exists: %v", err)
	}
	if status := controller.Status(); status.State != "" || status.Ready || status.Error != "" {
		t.Fatalf("Status() after Clear = %#v", status)
	}
}

func TestOpenAIOAuthHTTPRoutes(t *testing.T) {
	controller := NewOpenAIOAuth(filepath.Join(t.TempDir(), "openai-oauth.json"), oauthTestLogger())
	release := make(chan struct{})
	controller.login = func(ctx context.Context, options dopenai.OAuthOptions) (dopenai.CredentialSource, error) {
		if err := options.OpenURL("https://auth.example/authorize"); err != nil {
			return nil, err
		}
		select {
		case <-release:
			return staticOAuthCredentials{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s := &Server{openAIOAuth: controller}

	start := httptest.NewRecorder()
	s.handleOpenAIOAuthStart(start, httptest.NewRequest(http.MethodPost, "/api/auth/openai/start", nil))
	if start.Code != http.StatusAccepted || !strings.Contains(start.Body.String(), `"authorization_url":"https://auth.example/authorize"`) {
		t.Fatalf("start = %d %s", start.Code, start.Body.String())
	}
	statusResponse := httptest.NewRecorder()
	s.handleOpenAIOAuthStatus(statusResponse, httptest.NewRequest(http.MethodGet, "/api/auth/openai/status", nil))
	var status OpenAIOAuthStatus
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if statusResponse.Code != http.StatusOK || status.State != "pending" || status.ModelID != OpenAISubscriptionModelID {
		t.Fatalf("status = %d %#v", statusResponse.Code, status)
	}
	close(release)
	controller.Close()
}

func waitForOAuthState(t *testing.T, controller *OpenAIOAuth, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if controller.Status().State == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("OAuth state = %q, want %q", controller.Status().State, want)
}
