package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/semistrict/dago/damanaged"
)

type fakeMCPRegistry struct {
	servers     []damanaged.MCPServer
	created     damanaged.MCPServerOptions
	createdName string
	createdURL  string
	updated     damanaged.MCPServerPatch
	deleted     []string
	tools       []map[string]any
	provider    map[string]any
	sessions    []map[string]any
}

func (fake *fakeMCPRegistry) ListMCPServers(context.Context) ([]damanaged.MCPServer, error) {
	return fake.servers, nil
}
func (fake *fakeMCPRegistry) GetMCPServer(_ context.Context, id string) (damanaged.MCPServer, error) {
	for _, server := range fake.servers {
		if server["id"] == id {
			return server, nil
		}
	}
	return damanaged.MCPServer{"id": id}, nil
}
func (fake *fakeMCPRegistry) CreateMCPServer(_ context.Context, name, rawURL string, options damanaged.MCPServerOptions) (damanaged.MCPServer, error) {
	fake.createdName, fake.createdURL, fake.created = name, rawURL, options
	return damanaged.MCPServer{"id": "s1", "name": name, "url": rawURL}, nil
}
func (fake *fakeMCPRegistry) UpdateMCPServer(_ context.Context, id string, patch damanaged.MCPServerPatch) (damanaged.MCPServer, error) {
	fake.updated = patch
	return damanaged.MCPServer{"id": id, "name": "Fleet", "url": "https://new.example"}, nil
}
func (fake *fakeMCPRegistry) DeleteMCPServer(_ context.Context, id string) error {
	fake.deleted = append(fake.deleted, id)
	return nil
}
func (fake *fakeMCPRegistry) ListMCPServerTools(context.Context, string, string) ([]map[string]any, error) {
	return fake.tools, nil
}
func (fake *fakeMCPRegistry) RegisterMCPProvider(context.Context, string) (map[string]any, error) {
	return fake.provider, nil
}
func (fake *fakeMCPRegistry) CreateAuthSession(context.Context, string, []string, bool) (map[string]any, error) {
	if len(fake.sessions) == 0 {
		return nil, nil
	}
	return fake.sessions[0], nil
}
func (fake *fakeMCPRegistry) GetAuthSession(context.Context, string, int) (map[string]any, error) {
	if len(fake.sessions) < 2 {
		return nil, nil
	}
	session := fake.sessions[1]
	fake.sessions = fake.sessions[1:]
	return session, nil
}

func TestRunMCPServersAddParsesHeadersAndDefaultsName(t *testing.T) {
	fake := &fakeMCPRegistry{}
	var output bytes.Buffer
	err := runMCPServersWithClient(context.Background(), fake, []string{
		"add", "--header", "X-Api-Key=secret", "--no-tools", "https://tools.example",
	}, strings.NewReader(""), &output, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if fake.createdName != "tools.example" || fake.createdURL != "https://tools.example" || len(fake.created.Headers) != 1 || fake.created.Headers[0].Value != "secret" {
		t.Fatalf("name=%q url=%q options=%#v", fake.createdName, fake.createdURL, fake.created)
	}
	if strings.Contains(output.String(), "secret") || !strings.Contains(output.String(), "Created") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunMCPServersGetRedactsHeadersAndResolvesName(t *testing.T) {
	fake := &fakeMCPRegistry{servers: []damanaged.MCPServer{{
		"id": "s1", "name": "Fleet", "url": "https://tools.example", "headers": []any{map[string]any{"key": "Authorization", "value": "secret"}},
	}}}
	var output bytes.Buffer
	if err := runMCPServersWithClient(context.Background(), fake, []string{"get", "Fleet"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "secret") || !strings.Contains(output.String(), `"value": "***"`) {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunMCPServersDeleteConfirmsAndToolsPrintSnippet(t *testing.T) {
	fake := &fakeMCPRegistry{
		servers: []damanaged.MCPServer{{"id": "s1", "name": "Fleet", "url": "https://tools.example"}},
		tools:   []map[string]any{{"name": "read_url", "description": "Read a URL\nmore"}},
	}
	var output bytes.Buffer
	if err := runMCPServersWithClient(context.Background(), fake, []string{"delete", "Fleet"}, strings.NewReader("no\n"), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 || !strings.Contains(output.String(), "Aborted") {
		t.Fatalf("deleted=%v output=%q", fake.deleted, output.String())
	}
	output.Reset()
	if err := runMCPServersWithClient(context.Background(), fake, []string{"tools", "Fleet"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"mcp_server_url": "https://tools.example"`) || strings.Contains(output.String(), "\nmore") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestResolveMCPServerIDRejectsAmbiguity(t *testing.T) {
	fake := &fakeMCPRegistry{servers: []damanaged.MCPServer{
		{"id": "a", "name": "same", "url": "https://one.example"},
		{"id": "b", "name": "same", "url": "https://two.example"},
	}}
	if _, err := resolveMCPServerID(context.Background(), fake, "same"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunMCPConnectPrintsVerificationAndPollsWithoutOpening(t *testing.T) {
	fake := &fakeMCPRegistry{
		servers:  []damanaged.MCPServer{{"id": "s1", "name": "Fleet", "url": "https://tools.example"}},
		provider: map[string]any{"oauth_provider_id": "provider-1"},
		sessions: []map[string]any{
			{"id": "session-1", "status": "PENDING", "verification_url": "https://auth.example/verify"},
			{"id": "session-1", "status": "COMPLETED"},
		},
	}
	var output bytes.Buffer
	opened := false
	err := runMCPConnect(context.Background(), fake, []string{"--no-browser", "--timeout", "1", "Fleet"}, &output, &bytes.Buffer{}, func(string) error {
		opened = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if opened || !strings.Contains(output.String(), "https://auth.example/verify") || !strings.Contains(output.String(), "ready") {
		t.Fatalf("opened=%v output=%q", opened, output.String())
	}
}

func TestRunMCPServersAddCanConnectOAuthWithoutBrowser(t *testing.T) {
	fake := &fakeMCPRegistry{
		provider: map[string]any{"oauth_provider_id": "provider-1"},
		sessions: []map[string]any{{
			"id": "session-1", "status": "PENDING", "verification_url": "https://auth.example/verify",
		}},
	}
	var output bytes.Buffer
	err := runMCPServersWithClient(context.Background(), fake, []string{
		"add", "--auth-type", "oauth", "--connect", "--no-browser", "--timeout", "0", "https://tools.example",
	}, strings.NewReader(""), &output, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if fake.created.AuthType != "oauth" || fake.created.OAuthMode != "per_user_dynamic_client" {
		t.Fatalf("options = %#v", fake.created)
	}
	if !strings.Contains(output.String(), "Authorization started") || !strings.Contains(output.String(), "https://auth.example/verify") {
		t.Fatalf("output = %q", output.String())
	}
}
