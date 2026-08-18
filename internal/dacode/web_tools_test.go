package dacode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/daweb"
)

func TestBuildWebToolsAlwaysIncludesFetchAndGatesSearchOnTavily(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	lookup := func(string) (string, bool) { return "", false }
	tools, err := buildWebTools(t.Context(), path, lookup, daweb.NewClient(daweb.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := webToolNames(tools); got != "fetch_url" {
		t.Fatalf("tools = %q", got)
	}

	lookup = func(name string) (string, bool) {
		if name == "TAVILY_API_KEY" {
			return "environment-secret", true
		}
		return "", false
	}
	tools, err = buildWebTools(t.Context(), path, lookup, daweb.NewClient(daweb.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := webToolNames(tools); got != "fetch_url,web_search" {
		t.Fatalf("tools = %q", got)
	}
}

func TestBuildWebToolsUsesStoredPrecedenceAndFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	store := dacredential.NewStore(path, time.Now, dacredential.Options{})
	if err := store.SetAPIKey(t.Context(), "tavily", "stored-secret", "", ""); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) {
		if name == "TAVILY_API_KEY" {
			return "environment-secret", true
		}
		return "", false
	}
	tools, err := buildWebTools(t.Context(), path, lookup, daweb.NewClient(daweb.Options{}))
	if err != nil || webToolNames(tools) != "fetch_url,web_search" {
		t.Fatalf("tools = %q, error = %v", webToolNames(tools), err)
	}

	if err := store.SetOAuth(t.Context(), "tavily", "access", "refresh", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := buildWebTools(t.Context(), path, lookup, daweb.NewClient(daweb.Options{})); err == nil {
		t.Fatal("non-API-key Tavily credential was accepted")
	}
	if err := os.WriteFile(path, []byte(`{"version":99,"credentials":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildWebTools(t.Context(), path, lookup, daweb.NewClient(daweb.Options{})); !errors.Is(err, dacredential.ErrInvalidStore) {
		t.Fatalf("corrupt store error = %v", err)
	}
}

func TestBuildWebToolsHonorsCancellationAndStaticInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := buildWebTools(ctx, path, os.LookupEnv, daweb.NewClient(daweb.Options{})); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	for _, invoke := range []func(){
		func() { _, _ = buildWebTools(nil, path, os.LookupEnv, daweb.NewClient(daweb.Options{})) },
		func() { _, _ = buildWebTools(t.Context(), "", os.LookupEnv, daweb.NewClient(daweb.Options{})) },
		func() { _, _ = buildWebTools(t.Context(), path, nil, daweb.NewClient(daweb.Options{})) },
		func() { _, _ = buildWebTools(t.Context(), path, os.LookupEnv, nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected static input panic")
				}
			}()
			invoke()
		}()
	}
}

func TestWebSearchApprovalRuleIsExactAndCreditExplicit(t *testing.T) {
	rule := webSearchApprovalRule()
	matched, err := rule.MatchesName("web_search")
	if err != nil || !matched || rule.Description != "Allow this Tavily web search? This uses API credits." {
		t.Fatalf("rule = %#v, match = %t, error = %v", rule, matched, err)
	}
	if matched, err := rule.MatchesName("web_search_extra"); err != nil || matched {
		t.Fatalf("unexpected broader match = %t, error = %v", matched, err)
	}
	matched = false
	for _, candidate := range defaultToolApprovalRules() {
		if ok, matchErr := candidate.MatchesName("web_search"); matchErr != nil {
			t.Fatal(matchErr)
		} else if ok {
			matched = true
		}
	}
	if !matched {
		t.Fatal("default runner policy omitted web_search approval")
	}
}

func webToolNames(tools []datool.Tool) string {
	names := make([]string, len(tools))
	for index, tool := range tools {
		names[index] = tool.Definition().Name
	}
	return strings.Join(names, ",")
}
