package dacode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/daproviders/openai"
)

func TestAuthenticationStoredCredentialPrecedesEnvironmentAndOAuth(t *testing.T) {
	hooks := authenticationHooks{
		loadStored: func(context.Context) (dacredential.APIKeyCredential, bool, error) {
			return dacredential.APIKeyCredential{Key: "stored-secret", BaseURL: "https://stored.example/v1"}, true, nil
		},
		load: func(string, openai.OAuthOptions) (*openai.OAuthSession, error) {
			t.Fatal("OAuth load called after a stored credential resolved")
			return nil, nil
		},
		login: func(context.Context, func(string) error, openai.OAuthOptions) (*openai.OAuthSession, error) {
			t.Fatal("OAuth login called after a stored credential resolved")
			return nil, nil
		},
	}
	authentication, err := resolveAuthentication(t.Context(), "environment-secret", t.TempDir(), io.Discard, hooks)
	if err != nil {
		t.Fatal(err)
	}
	if authentication.apiKey != "stored-secret" || !authentication.hasPairedURL || authentication.pairedURL != "https://stored.example/v1" || authentication.subscription {
		t.Fatalf("authentication = %#v", authentication)
	}
}

func TestAuthenticationStoredCredentialWithoutEndpointClearsInheritedEndpoint(t *testing.T) {
	hooks := authenticationHooks{
		loadStored: func(context.Context) (dacredential.APIKeyCredential, bool, error) {
			return dacredential.APIKeyCredential{Key: "stored-secret"}, true, nil
		},
	}
	authentication, err := resolveAuthentication(t.Context(), "environment-secret", t.TempDir(), io.Discard, hooks)
	if err != nil {
		t.Fatal(err)
	}
	if !authentication.hasPairedURL || authentication.pairedURL != "" {
		t.Fatalf("paired endpoint = %#v", authentication)
	}
	if _, err := authentication.newModel("main-model", "https://inherited-gateway.example/v1"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadOpenAICredentialAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	store := dacredential.NewStore(path, time.Now, dacredential.Options{})
	if err := store.SetAPIKey(t.Context(), "openai", "stored-secret", "https://gateway.example/v1", ""); err != nil {
		t.Fatal(err)
	}
	credential, ok, err := loadOpenAICredential(t.Context(), path)
	if err != nil || !ok || credential.Key != "stored-secret" || credential.BaseURL != "https://gateway.example/v1" {
		t.Fatalf("load = %#v, %t, %v", credential, ok, err)
	}
	if err := os.WriteFile(path, []byte(`{"version":99,"credentials":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = loadOpenAICredential(t.Context(), path)
	if !errors.Is(err, dacredential.ErrInvalidStore) || strings.Contains(err.Error(), "stored-secret") {
		t.Fatalf("corrupt error = %v", err)
	}
}
