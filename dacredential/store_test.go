package dacredential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var fixedTime = time.Date(2026, 8, 17, 12, 30, 45, 123, time.FixedZone("offset", 3600))

func TestStoreRoundTripsDiscriminatedCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "auth.json")
	store := NewStore(path, func() time.Time { return fixedTime }, Options{})
	if err := store.SetAPIKey(t.Context(), "openai", "  sk-secret\n", "https://gateway.example/v1", ""); err != nil {
		t.Fatal(err)
	}
	expires := fixedTime.Add(time.Hour)
	if err := store.SetOAuth(t.Context(), "openai_oauth", "access", "refresh", expires); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(snapshot.Providers(), ","); got != "openai,openai_oauth" {
		t.Fatalf("Providers = %q", got)
	}
	apiKey, ok := snapshot.APIKey("openai")
	if !ok || apiKey.Key != "sk-secret" || apiKey.BaseURL != "https://gateway.example/v1" || !apiKey.AddedAt.Equal(fixedTime.UTC().Truncate(time.Second)) {
		t.Fatalf("API key = %#v, %t", apiKey, ok)
	}
	oauth, ok := snapshot.Credential("openai_oauth")
	if !ok || oauth.Type != OAuthType || oauth.OAuth == nil || oauth.OAuth.AccessToken != "access" || !oauth.OAuth.ExpiresAt.Equal(expires.Truncate(time.Second)) {
		t.Fatalf("OAuth = %#v, %t", oauth, ok)
	}
	for _, rendered := range []string{fmt.Sprint(apiKey), fmt.Sprintf("%#v", apiKey), fmt.Sprint(oauth), fmt.Sprintf("%#v", oauth)} {
		if strings.Contains(rendered, "sk-secret") || strings.Contains(rendered, "access") || strings.Contains(rendered, "refresh") {
			t.Fatalf("formatted credential leaked a secret: %s", rendered)
		}
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	if document["version"] != float64(1) {
		t.Fatalf("document = %#v", document)
	}
	if runtime.GOOS != "windows" {
		fileInfo, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		parentInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if fileInfo.Mode().Perm() != 0o600 || parentInfo.Mode().Perm() != 0o700 {
			t.Fatalf("modes = %o/%o", fileInfo.Mode().Perm(), parentInfo.Mode().Perm())
		}
	}
}

func TestSetReplacesMetadataAndRemoveIsIdempotent(t *testing.T) {
	store := testStore(t)
	if err := store.SetAPIKey(t.Context(), "langsmith", "first", "https://eu.api.smith.langchain.com", "project-one"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(t.Context(), "langsmith", "second", "", ""); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	credential, ok := snapshot.APIKey("langsmith")
	if !ok || credential.Key != "second" || credential.BaseURL != "" || credential.Project != "" {
		t.Fatalf("replacement = %#v", credential)
	}
	removed, err := store.Remove(t.Context(), "langsmith")
	if err != nil || !removed {
		t.Fatalf("Remove = %t, %v", removed, err)
	}
	removed, err = store.Remove(t.Context(), "langsmith")
	if err != nil || removed {
		t.Fatalf("second Remove = %t, %v", removed, err)
	}
}

func TestStoreValidationAndMalformedRecordsFailClosed(t *testing.T) {
	store := testStore(t)
	for _, test := range []struct {
		name, provider, key, baseURL, project string
	}{
		{name: "empty provider", provider: "", key: "key"},
		{name: "hostile provider", provider: "../openai", key: "key"},
		{name: "empty key", provider: "openai", key: "  "},
		{name: "header injection", provider: "openai", key: "key\r\nInjected: value"},
		{name: "credential URL", provider: "openai", key: "key", baseURL: "https://user:" + "pass@example.test"},
		{name: "non HTTP URL", provider: "openai", key: "key", baseURL: "file:///tmp/key"},
		{name: "foreign project", provider: "openai", key: "key", project: "project"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := store.SetAPIKey(t.Context(), test.provider, test.key, test.baseURL, test.project)
			if !errors.Is(err, ErrInvalidCredential) {
				t.Fatalf("SetAPIKey error = %v", err)
			}
		})
	}

	path := store.Path()
	mustWrite(t, path, `{"version":1,"credentials":{`+
		`"good":{"type":"api_key","key":"safe","added_at":"2026-08-17T11:30:45Z"},`+
		`"empty":{"type":"api_key","key":"","added_at":"2026-08-17T11:30:45Z"},`+
		`"future":{"type":"future","value":"secret"},`+
		`"bad":{"type":"api_key","key":"secret","added_at":"not-time"}}}`)
	snapshot, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(snapshot.Providers(), ","); got != "good" || snapshot.Malformed() != 3 {
		t.Fatalf("snapshot = providers %q malformed %d", got, snapshot.Malformed())
	}
}

func TestStoreRejectsCorruptUnsupportedOversizedAndLinkedFiles(t *testing.T) {
	tests := []struct {
		name, payload string
	}{
		{name: "invalid JSON", payload: `{not-json`},
		{name: "non object", payload: `[]`},
		{name: "future version", payload: `{"version":2,"credentials":{}}`},
		{name: "trailing", payload: `{"version":1,"credentials":{}} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			mustWrite(t, store.Path(), test.payload)
			_, err := store.Load(t.Context())
			if !errors.Is(err, ErrInvalidStore) {
				t.Fatalf("Load error = %v", err)
			}
		})
	}

	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "auth.json")
		mustWrite(t, path, strings.Repeat("x", 33))
		store := NewStore(path, time.Now, Options{MaxFileBytes: 32})
		_, err := store.Load(t.Context())
		if !errors.Is(err, ErrInvalidStore) && !errors.Is(err, ErrCredentialLimit) {
			t.Fatalf("Load error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation generally needs privileges")
		}
		dir := t.TempDir()
		target := filepath.Join(dir, "real.json")
		link := filepath.Join(dir, "auth.json")
		mustWrite(t, target, `{"version":1,"credentials":{}}`)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		store := NewStore(link, time.Now, Options{})
		if _, err := store.Load(t.Context()); !errors.Is(err, ErrInvalidStore) {
			t.Fatalf("Load error = %v", err)
		}
		if err := store.SetAPIKey(t.Context(), "openai", "secret", "", ""); !errors.Is(err, ErrInvalidStore) {
			t.Fatalf("SetAPIKey error = %v", err)
		}
	})
}

func TestStoreCancellationAtomicityAndConcurrency(t *testing.T) {
	t.Run("cancelled lock wait", func(t *testing.T) {
		store := testStore(t)
		<-store.lock
		defer store.release()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.Load(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Load error = %v", err)
		}
		if err := store.SetAPIKey(ctx, "openai", "secret", "", ""); !errors.Is(err, context.Canceled) {
			t.Fatalf("SetAPIKey error = %v", err)
		}
	})

	t.Run("cancel preserves old file", func(t *testing.T) {
		store := testStore(t)
		if err := store.SetAPIKey(t.Context(), "openai", "original", "", ""); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := store.SetAPIKey(ctx, "anthropic", "second", "", ""); !errors.Is(err, context.Canceled) {
			t.Fatalf("SetAPIKey error = %v", err)
		}
		snapshot, err := store.Load(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := snapshot.APIKey("openai"); !ok || len(snapshot.Providers()) != 1 {
			t.Fatalf("snapshot = %#v", snapshot.Providers())
		}
	})

	t.Run("concurrent providers", func(t *testing.T) {
		store := testStore(t)
		const count = 32
		var wait sync.WaitGroup
		errorsSeen := make(chan error, count)
		for index := range count {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				errorsSeen <- store.SetAPIKey(context.Background(), fmt.Sprintf("provider-%02d", index), "secret", "", "")
			}(index)
		}
		wait.Wait()
		close(errorsSeen)
		for err := range errorsSeen {
			if err != nil {
				t.Fatal(err)
			}
		}
		snapshot, err := store.Load(t.Context())
		if err != nil || len(snapshot.Providers()) != count {
			t.Fatalf("Load = %d providers, %v", len(snapshot.Providers()), err)
		}
	})
}

func TestStoreErrorsAndFormattingNeverLeakSecrets(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"), time.Now, Options{MaxSecretBytes: 8})
	secret := "SECRET-DO-NOT-PRINT"
	err := store.SetAPIKey(t.Context(), "openai", secret, "", "")
	if !errors.Is(err, ErrInvalidCredential) || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v", err)
	}
	credential := Credential{Type: OAuthType, OAuth: &OAuthCredential{AccessToken: secret, RefreshToken: secret}}
	if rendered := fmt.Sprintf("%#v %v", credential, credential); strings.Contains(rendered, secret) {
		t.Fatalf("rendered credential leaked: %s", rendered)
	}
}

func TestNewStoreRejectsInvalidStaticInputs(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		now  func() time.Time
		opts Options
	}{
		{name: "path", now: time.Now},
		{name: "clock", path: "auth.json"},
		{name: "bounds", path: "auth.json", now: time.Now, opts: Options{MaxCredentials: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewStore did not panic")
				}
			}()
			_ = NewStore(test.path, test.now, test.opts)
		})
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "state", "auth.json"), func() time.Time { return fixedTime }, Options{})
}

func mustWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}
