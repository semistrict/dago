package dacode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dacredential"
)

func TestAuthCommandSetStatusListRemoveAndPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "auth.json")
	stateDirectory := t.TempDir()
	secret := "stored-private-value"
	var output bytes.Buffer
	if err := runAuthCommand(t.Context(), []string{"set", "openai", "--auth-file", path}, strings.NewReader(secret+"\n"), &output, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), "Stored credential for openai") {
		t.Fatalf("set output = %q", output.String())
	}
	t.Setenv("OPENAI_API_KEY", "environment-private-value")
	output.Reset()
	if err := runAuthCommand(t.Context(), []string{"status", "openai", "--auth-file", path}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "openai\tstored\n" || strings.Contains(got, "private") {
		t.Fatalf("status output = %q", got)
	}
	output.Reset()
	if err := runAuthCommand(t.Context(), []string{"list", "--json", "--auth-file=" + path, "--state-dir", stateDirectory}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), `"provider":"tavily"`) || !strings.Contains(output.String(), `"provider":"langsmith"`) {
		t.Fatalf("list output = %q", output.String())
	}
	output.Reset()
	if err := runAuthCommand(t.Context(), []string{"path", "--json", "--auth-file", path}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"exists":true`) || !strings.Contains(output.String(), filepath.Base(path)) {
		t.Fatalf("path output = %q", output.String())
	}
	output.Reset()
	if err := runAuthCommand(t.Context(), []string{"remove", "openai", "--json", "--auth-file", path}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), `"removed":true`) {
		t.Fatalf("remove output = %q", output.String())
	}
}

func TestAuthCommandFromEnvironmentAndMetadataReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("AUTH_TEST_KEY", "environment-secret")
	if err := runAuthCommand(t.Context(), []string{
		"set", "langsmith", "--from-env", "AUTH_TEST_KEY", "--base-url", "eu", "--project", "project-one", "--auth-file", path,
	}, strings.NewReader("ignored"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	store := dacredential.NewStore(path, time.Now, dacredential.Options{})
	snapshot, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	credential, ok := snapshot.APIKey("langsmith")
	if !ok || credential.Key != "environment-secret" || credential.BaseURL != "https://eu.api.smith.langchain.com" || credential.Project != "project-one" {
		t.Fatalf("credential = %#v", credential)
	}
	t.Setenv("AUTH_TEST_KEY", "replacement-secret")
	if err := runAuthCommand(t.Context(), []string{
		"set", "langsmith", "--from-env=AUTH_TEST_KEY", "--auth-file", path,
	}, strings.NewReader("ignored"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = store.Load(t.Context())
	credential, _ = snapshot.APIKey("langsmith")
	if credential.Key != "replacement-secret" || credential.BaseURL != "https://eu.api.smith.langchain.com" || credential.Project != "project-one" {
		t.Fatalf("preserved metadata = %#v", credential)
	}
	if err := runAuthCommand(t.Context(), []string{
		"set", "langsmith", "--from-env", "AUTH_TEST_KEY", "--base-url=", "--project=", "--auth-file", path,
	}, strings.NewReader("ignored"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = store.Load(t.Context())
	credential, _ = snapshot.APIKey("langsmith")
	if credential.BaseURL != "" || credential.Project != "" {
		t.Fatalf("cleared metadata = %#v", credential)
	}
}

func TestAuthCommandReportsTavilyAndLangSmithServiceSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	stateDirectory := t.TempDir()
	t.Setenv("DEEPAGENTS_CODE_TAVILY_API_KEY", "prefixed-service-secret")
	t.Setenv("LANGSMITH_API_KEY", "environment-service-secret")
	var output bytes.Buffer
	if err := runAuthCommand(t.Context(), []string{"status", "tavily", "--json", "--auth-file", path}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); strings.Contains(got, "service-secret") || !strings.Contains(got, `"environment":"DEEPAGENTS_CODE_TAVILY_API_KEY"`) || !strings.Contains(got, `"service":true`) {
		t.Fatalf("Tavily status = %s", got)
	}
	output.Reset()
	if err := runAuthCommand(t.Context(), []string{"status", "langsmith", "--json", "--auth-file", path}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); strings.Contains(got, "service-secret") || !strings.Contains(got, `"environment":"LANGSMITH_API_KEY"`) || !strings.Contains(got, `"service":true`) {
		t.Fatalf("LangSmith status = %s", got)
	}
	if err := runAuthCommand(t.Context(), []string{"set", "tavily", "--auth-file", path}, strings.NewReader("stored-service-secret\n"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runAuthCommand(t.Context(), []string{"list", "--json", "--auth-file", path, "--state-dir", stateDirectory}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); strings.Contains(got, "service-secret") || !strings.Contains(got, `"provider":"tavily","status":"stored","type":"api_key","service":true`) {
		t.Fatalf("service list = %s", got)
	}
}

func TestAuthCommandRejectsSecretsInArgumentsAndUnsafeInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	secret := "must-not-appear"
	tests := []struct {
		name      string
		arguments []string
		input     string
	}{
		{name: "secret argument", arguments: []string{"set", "openai", secret, "--auth-file", path}},
		{name: "empty stdin", arguments: []string{"set", "openai", "--auth-file", path}},
		{name: "OAuth only", arguments: []string{"set", "openai_oauth", "--auth-file", path}, input: secret},
		{name: "foreign project", arguments: []string{"set", "openai", "--project", "x", "--auth-file", path}, input: secret},
		{name: "invalid env", arguments: []string{"set", "openai", "--from-env", "BAD-NAME", "--auth-file", path}},
		{name: "missing status provider", arguments: []string{"status", "--auth-file", path}},
		{name: "irrelevant state path", arguments: []string{"set", "openai", "--state-dir", t.TempDir(), "--auth-file", path}, input: secret},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err := runAuthCommand(t.Context(), test.arguments, strings.NewReader(test.input), &output, &output)
			if err == nil {
				t.Fatal("unsafe input accepted")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(output.String(), secret) {
				t.Fatalf("secret leaked: err=%v output=%q", err, output.String())
			}
		})
	}
	oversized := strings.Repeat("x", dacredential.DefaultOptions().MaxSecretBytes+1)
	if err := runAuthCommand(t.Context(), []string{"set", "openai", "--auth-file", path}, strings.NewReader(oversized), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("oversized secret accepted")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runAuthCommand(cancelled, []string{"list", "--auth-file", path}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestAuthCommandJSONNeverContainsStoredCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	store := dacredential.NewStore(path, time.Now, dacredential.Options{})
	if err := store.SetOAuth(t.Context(), "openai_oauth", "access-private", "refresh-private", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runAuthCommand(t.Context(), []string{"status", "openai_oauth", "--json", "--auth-file", path}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "access-private") || strings.Contains(output.String(), "refresh-private") {
		t.Fatalf("JSON leaked OAuth tokens: %s", output.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
}

func TestAuthCommandReportsAndRemovesExistingOAuthSignIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	stateDirectory := t.TempDir()
	oauthPath := filepath.Join(stateDirectory, oauthStoreFilename)
	if err := os.WriteFile(oauthPath, []byte(`{"access_token":"access-private","refresh_token":"refresh-private"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	arguments := []string{"status", "openai_oauth", "--json", "--auth-file", path, "--state-dir", stateDirectory}
	if err := runAuthCommand(t.Context(), arguments, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "private") || !strings.Contains(output.String(), `"status":"stored"`) || !strings.Contains(output.String(), `"type":"oauth"`) {
		t.Fatalf("OAuth status = %s", output.String())
	}
	output.Reset()
	if err := runAuthCommand(t.Context(), []string{"remove", "openai_oauth", "--auth-file", path, "--state-dir", stateDirectory}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(oauthPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OAuth file still exists: %v", err)
	}
	if output.String() != "Removed stored credential for openai_oauth.\n" {
		t.Fatalf("remove output = %q", output.String())
	}
}

func TestRunDispatchesAuthWithoutStartingAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	var stdout, stderr bytes.Buffer
	if err := Run(t.Context(), []string{"auth", "set", "openai", "--auth-file", path}, strings.NewReader("dispatch-secret\n"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String()+stderr.String(), "dispatch-secret") {
		t.Fatalf("dispatch leaked secret: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	if err := Run(t.Context(), []string{"auth", "status", "openai", "--auth-file", path}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "openai\tstored\n" {
		t.Fatalf("status = %q", stdout.String())
	}
	stdout.Reset()
	printUsage(&stdout)
	if !strings.Contains(stdout.String(), "dacode auth [COMMAND]") {
		t.Fatalf("usage missing auth command:\n%s", stdout.String())
	}
}
