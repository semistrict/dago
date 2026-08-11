package contexthub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	ls "github.com/langchain-ai/langsmith-go"
	"github.com/langchain-ai/langsmith-go/option"
)

func TestLangSmithClientPullDecodesFilesAndLinks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/platform/hub/repos/-/agent/directories" || request.URL.Query().Get("commit") != "abc12345" {
			t.Errorf("request = %s %s", request.Method, request.URL.String())
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"commit_hash": "abc12345",
			"files": map[string]any{
				"a.md":     map[string]any{"type": "file", "content": "hello"},
				"skills/s": map[string]any{"type": "skill", "repo_handle": "s"},
			},
		})
	}))
	defer server.Close()
	client, err := NewLangSmithClient(ls.NewClient(option.WithBaseURL(server.URL)))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := client.Pull(context.Background(), "-/agent:abc12345")
	if err != nil {
		t.Fatal(err)
	}
	if tree.CommitHash != "abc12345" || tree.Entries["a.md"].Content != "hello" || tree.Entries["skills/s"].RepoHandle != "s" {
		t.Fatalf("tree = %#v", tree)
	}
}

func TestLangSmithClientPullMapsNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"detail":"missing"}`))
	}))
	defer server.Close()
	client, _ := NewLangSmithClient(ls.NewClient(option.WithBaseURL(server.URL)))
	_, err := client.Pull(context.Background(), "-/missing")
	if err != ErrRepositoryNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestLangSmithClientPushCreatesPrivateAgentThenCommits(t *testing.T) {
	var mu sync.Mutex
	var requests []struct {
		method string
		path   string
		body   map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if request.Body != nil {
			_ = json.NewDecoder(request.Body).Decode(&body)
		}
		mu.Lock()
		requests = append(requests, struct {
			method string
			path   string
			body   map[string]any
		}{request.Method, request.URL.Path, body})
		mu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/repos/-/new-agent":
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte(`{"detail":"missing"}`))
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/repos":
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{"repo": map[string]any{}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/platform/hub/repos/-/new-agent/directories/commits":
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{"commit": map[string]any{"commit_hash": "newhash"}})
		default:
			http.Error(writer, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client, _ := NewLangSmithClient(ls.NewClient(option.WithBaseURL(server.URL)))
	result, err := client.Push(context.Background(), "-/new-agent", map[string]*Entry{
		"a.md": {Kind: EntryFile, Content: "hello"},
		"gone": nil,
	}, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if result.CommitHash != "newhash" || len(requests) != 3 {
		t.Fatalf("result = %#v, requests = %#v", result, requests)
	}
	if requests[1].body["repo_handle"] != "new-agent" || requests[1].body["repo_type"] != "agent" || requests[1].body["is_public"] != false {
		t.Fatalf("create body = %#v", requests[1].body)
	}
	if requests[2].body["parent_commit"] != "parent" {
		t.Fatalf("commit body = %#v", requests[2].body)
	}
	files := requests[2].body["files"].(map[string]any)
	if files["gone"] != nil || files["a.md"].(map[string]any)["content"] != "hello" {
		t.Fatalf("files = %#v", files)
	}
}

func TestParseIdentifier(t *testing.T) {
	owner, name, version, err := parseIdentifier("owner/name:abc")
	if err != nil || owner != "owner" || name != "name" || version != "abc" {
		t.Fatalf("got %q %q %q, %v", owner, name, version, err)
	}
	for _, invalid := range []string{"", "name", "/name", "owner/", "a/b/extra", "a/b:"} {
		if _, _, _, err := parseIdentifier(invalid); err == nil {
			t.Fatalf("%q accepted", invalid)
		}
	}
}

func TestNewLangSmithUsesDefaultClient(t *testing.T) {
	remote, err := NewLangSmith("-/agent", nil)
	if err != nil || remote == nil {
		t.Fatalf("backend = %#v, err = %v", remote, err)
	}
}
