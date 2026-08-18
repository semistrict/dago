package tracing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/dago/datalon"
)

type credentialStore struct {
	credential Credential
	err        error
	calls      int
	panicValue any
}

func (store *credentialStore) LoadTracingCredential(ctx context.Context) (Credential, error) {
	store.calls++
	if store.panicValue != nil {
		panic(store.panicValue)
	}
	return store.credential, store.err
}

func TestManagerStoredCredentialEnablesIsolatedTracing(t *testing.T) {
	t.Parallel()
	secret := "stored-secret-value"
	store := &credentialStore{credential: NewCredential(secret, "eu", "agent-project")}
	manager := NewManager(store, ManagementOptions{})
	environment := map[string]string{
		"LANGSMITH_PROJECT": "user-project", "LANGSMITH_TRACING": "false",
		"LANGSMITH_API_KEY": "user-shell-key", "UNRELATED": "kept",
	}
	configuration, err := manager.Resolve(t.Context(), environment)
	if err != nil {
		t.Fatal(err)
	}
	status := configuration.Status()
	if status.Enabled || !status.ExplicitlyDisabled || status.Project != "user-project" || status.Endpoint != defaultEUEndpoint || store.calls != 1 {
		t.Fatalf("status = %#v, calls = %d", status, store.calls)
	}
	if strings.Contains(fmt.Sprintf("%#v", store.credential), secret) || strings.Contains(status.String(), secret) {
		t.Fatal("secret appeared in a printable credential or status")
	}

	environment = map[string]string{"LANGSMITH_PROJECT": "user-project", "UNRELATED": "kept"}
	configuration, err = manager.Resolve(t.Context(), environment)
	if err != nil {
		t.Fatal(err)
	}
	status = configuration.Status()
	if !status.Enabled || status.Project != "user-project" || !status.HasCredentials {
		t.Fatalf("stored credential was not enabled: %#v", status)
	}
	agent := configuration.AgentEnvironment()
	if agent["LANGSMITH_API_KEY"] != secret || agent["LANGSMITH_PROJECT"] != "user-project" || agent["LANGSMITH_ENDPOINT"] != defaultEUEndpoint || agent["LANGSMITH_TRACING"] != "true" {
		t.Fatalf("agent environment = %#v", agent)
	}
	shell := configuration.ShellEnvironment(agent)
	if shell["LANGSMITH_PROJECT"] != "user-project" || shell["UNRELATED"] != "kept" {
		t.Fatalf("shell environment = %#v", shell)
	}
	if _, exists := shell["LANGSMITH_API_KEY"]; exists {
		t.Fatal("agent credential leaked into restored shell environment")
	}
}

func TestManagerPrefixedOverridesReplicasAndOrphanFailClosed(t *testing.T) {
	t.Parallel()
	manager := NewManager(&credentialStore{credential: NewCredential("stored", "", "stored-project")}, ManagementOptions{})
	configuration, err := manager.Resolve(t.Context(), map[string]string{
		"LANGSMITH_TRACING":                          "true",
		"DEEPAGENTS_CODE_LANGSMITH_TRACING":          "false",
		"DEEPAGENTS_CODE_LANGSMITH_API_KEY":          "",
		"LANGSMITH_API_KEY":                          "canonical",
		"DEEPAGENTS_CODE_LANGSMITH_PROJECT":          "prefixed-project",
		"DEEPAGENTS_CODE_LANGSMITH_REPLICA_PROJECTS": "one,two,one",
	})
	if err != nil {
		t.Fatal(err)
	}
	status := configuration.Status()
	if status.Enabled || !status.ExplicitlyDisabled || status.HasCredentials || status.Project != "prefixed-project" || strings.Join(status.ReplicaProjects, ",") != "one,two" {
		t.Fatalf("prefixed status = %#v", status)
	}
	if _, exists := configuration.AgentEnvironment()["LANGSMITH_API_KEY"]; exists {
		t.Fatal("empty prefixed key did not suppress the canonical key")
	}

	empty := NewStaticCredentialStore(Credential{})
	configuration, err = NewManager(empty, ManagementOptions{}).Resolve(t.Context(), map[string]string{"LANGSMITH_TRACING": "true"})
	if err != nil {
		t.Fatal(err)
	}
	status = configuration.Status()
	if status.Enabled || !status.Orphaned || status.HasCredentials {
		t.Fatalf("orphaned tracing did not fail closed: %#v", status)
	}
	configuration, err = NewManager(empty, ManagementOptions{}).Resolve(t.Context(), map[string]string{"LANGSMITH_TRACING": "true", "LANGSMITH_ENDPOINT": "http://127.0.0.1:1984"})
	if err != nil || !configuration.Status().Enabled {
		t.Fatalf("explicit local endpoint was not enabled: status=%#v err=%v", configuration.Status(), err)
	}
	configuration, err = NewManager(empty, ManagementOptions{}).Resolve(t.Context(), map[string]string{"LANGSMITH_TRACING": "true", "LANGSMITH_ENDPOINT": "https://trace.example/private/path"})
	if err != nil || configuration.Status().Endpoint != "https://trace.example" || configuration.AgentEnvironment()["LANGSMITH_ENDPOINT"] != "https://trace.example/private/path" {
		t.Fatalf("endpoint redaction mismatch: status=%#v err=%v", configuration.Status(), err)
	}
}

func TestManagerRejectsUnsafeInputsAndSanitizesStoreErrors(t *testing.T) {
	t.Parallel()
	secret := "credential-in-error"
	manager := NewManager(&credentialStore{err: errors.New(secret)}, ManagementOptions{})
	_, err := manager.Resolve(t.Context(), map[string]string{})
	if !errors.Is(err, ErrCredentialStore) || strings.Contains(err.Error(), secret) {
		t.Fatalf("store error = %v", err)
	}
	_, err = NewManager(&credentialStore{panicValue: secret}, ManagementOptions{}).Resolve(t.Context(), map[string]string{})
	if !errors.Is(err, ErrCredentialStore) || strings.Contains(err.Error(), secret) {
		t.Fatalf("store panic = %v", err)
	}
	for _, environment := range []map[string]string{
		{"LANGSMITH_ENDPOINT": "https://user:secret@example.com", "LANGSMITH_TRACING": "true"},
		{"LANGSMITH_ENDPOINT": "http://example.com", "LANGSMITH_TRACING": "true"},
		{"DEEPAGENTS_CODE_LANGSMITH_REPLICA_PROJECTS": "valid,bad\nproject"},
	} {
		if _, err := NewManager(NewStaticCredentialStore(Credential{}), ManagementOptions{}).Resolve(t.Context(), environment); !errors.Is(err, ErrInvalidTracing) {
			t.Fatalf("unsafe environment %#v returned %v", environment, err)
		}
	}
}

func TestManagedRuntimeRedactsAndReplicates(t *testing.T) {
	t.Parallel()
	secret := "high-entropy-secret"
	manager := NewManager(&credentialStore{credential: NewCredential(secret, "", "primary")}, ManagementOptions{})
	configuration, err := manager.Resolve(t.Context(), map[string]string{"DEEPAGENTS_CODE_LANGSMITH_REPLICA_PROJECTS": "replica-a,replica-b"})
	if err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{}
	runtime := NewManaged(&fakeRuntime{result: datalon.Result{Text: "answer " + secret}}, sink, "assistant", configuration, Options{})
	_, err = runtime.Invoke(t.Context(), datalon.Request{Text: "input " + secret, Metadata: map[string]any{"nested": map[string]any{"key": secret}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.runs) != 3 || len(sink.completions) != 3 {
		t.Fatalf("runs=%d completions=%d", len(sink.runs), len(sink.completions))
	}
	projects := []string{sink.runs[0].Project, sink.runs[1].Project, sink.runs[2].Project}
	if strings.Join(projects, ",") != "primary,replica-a,replica-b" {
		t.Fatalf("projects = %v", projects)
	}
	for _, run := range sink.runs {
		encoded := fmt.Sprintf("%#v", run)
		if strings.Contains(encoded, secret) || !strings.Contains(encoded, redactedValue) {
			t.Fatalf("run was not redacted: %s", encoded)
		}
	}
	for _, completion := range sink.completions {
		if strings.Contains(completion.Output, secret) || !strings.Contains(completion.Output, redactedValue) {
			t.Fatalf("completion was not redacted: %#v", completion)
		}
	}
}

func TestManagedRuntimeDoesNotExposeSinkErrors(t *testing.T) {
	t.Parallel()
	secret := "provider-secret-error"
	configuration, err := NewManager(&credentialStore{credential: NewCredential("key", "", "project")}, ManagementOptions{}).Resolve(t.Context(), map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	reported := []error{}
	runtime := NewManaged(&fakeRuntime{}, &fakeSink{beginErr: errors.New(secret)}, "assistant", configuration, Options{OnError: func(err error) { reported = append(reported, err) }})
	_, _ = runtime.Invoke(t.Context(), datalon.Request{})
	if len(reported) != 1 || strings.Contains(reported[0].Error(), secret) {
		t.Fatalf("reported errors = %v", reported)
	}
}

type sinkFactory struct {
	endpoint, key string
	sink          Sink
	err           error
	calls         int
	panicValue    any
}

func (factory *sinkFactory) NewTracingSink(ctx context.Context, endpoint, key string) (Sink, error) {
	factory.calls++
	factory.endpoint, factory.key = endpoint, key
	if factory.panicValue != nil {
		panic(factory.panicValue)
	}
	return factory.sink, factory.err
}

func TestConfigurationResolvesProviderSinkWithoutExposingFactoryErrors(t *testing.T) {
	t.Parallel()
	configuration, err := NewManager(&credentialStore{credential: NewCredential("secret", "eu", "project")}, ManagementOptions{}).Resolve(t.Context(), map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	factory := &sinkFactory{sink: &fakeSink{}}
	resolved, err := configuration.ResolveSink(t.Context(), factory)
	if err != nil || nilValue(resolved) || factory.endpoint != defaultEUEndpoint || factory.key != "secret" || factory.calls != 1 {
		t.Fatalf("sink resolution mismatch: err=%v calls=%d endpoint=%q", err, factory.calls, factory.endpoint)
	}
	factory = &sinkFactory{err: errors.New("secret provider detail")}
	_, err = configuration.ResolveSink(t.Context(), factory)
	if !errors.Is(err, ErrSinkFactory) || strings.Contains(err.Error(), "secret provider detail") {
		t.Fatalf("factory error = %v", err)
	}
	factory = &sinkFactory{panicValue: "secret provider panic"}
	_, err = configuration.ResolveSink(t.Context(), factory)
	if !errors.Is(err, ErrSinkFactory) || strings.Contains(err.Error(), "secret provider panic") {
		t.Fatalf("factory panic = %v", err)
	}
	disabled, err := NewManager(NewStaticCredentialStore(Credential{}), ManagementOptions{}).Resolve(t.Context(), map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	factory = &sinkFactory{err: errors.New("must not be called")}
	if _, err := disabled.ResolveSink(t.Context(), factory); err != nil || factory.calls != 0 {
		t.Fatalf("disabled sink resolution called factory: err=%v calls=%d", err, factory.calls)
	}
	keyless, err := NewManager(NewStaticCredentialStore(Credential{}), ManagementOptions{}).Resolve(t.Context(), map[string]string{
		"LANGSMITH_TRACING":        "true",
		"LANGSMITH_RUNS_ENDPOINTS": `{"https://replica.example":"replica-secret"}`,
	})
	if err != nil || !keyless.Status().Enabled {
		t.Fatalf("keyless replica configuration: status=%#v err=%v", keyless.Status(), err)
	}
	factory = &sinkFactory{sink: &fakeSink{}}
	if _, err := keyless.ResolveSink(t.Context(), factory); err != nil || factory.key != "" {
		t.Fatalf("replica key was used as primary key: err=%v", err)
	}
	if got := keyless.redact("replica-secret and replica-secret-long"); strings.Contains(got, "replica-secret") {
		t.Fatalf("replica secret was not redacted: %q", got)
	}
}

type projectLookup struct {
	mu         sync.Mutex
	url        string
	err        error
	calls      int
	started    chan struct{}
	release    chan struct{}
	panicValue any
}

func (lookup *projectLookup) ProjectURL(ctx context.Context, project string) (string, error) {
	lookup.mu.Lock()
	lookup.calls++
	lookup.mu.Unlock()
	if lookup.panicValue != nil {
		panic(lookup.panicValue)
	}
	if lookup.started != nil {
		select {
		case lookup.started <- struct{}{}:
		default:
		}
	}
	if lookup.release != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-lookup.release:
		}
	}
	return lookup.url, lookup.err
}

func TestURLResolverCachesAndBuildsSafeThreadLinks(t *testing.T) {
	t.Parallel()
	lookup := &projectLookup{url: "https://smith.example/o/org/projects/p/project/"}
	resolver := NewURLResolver(lookup, URLResolverOptions{})
	link, err := resolver.ThreadURL(t.Context(), "project", "thread-123")
	if err != nil || link != "https://smith.example/o/org/projects/p/project/t/thread-123?utm_source=dago" {
		t.Fatalf("link=%q err=%v", link, err)
	}
	cached, ok := resolver.CachedThreadURL("project", "thread-456")
	if !ok || !strings.Contains(cached, "/t/thread-456?") || lookup.calls != 1 {
		t.Fatalf("cached=%q ok=%t calls=%d", cached, ok, lookup.calls)
	}
	if _, err := resolver.ThreadURL(t.Context(), "project", "../escape"); !errors.Is(err, ErrProjectURL) {
		t.Fatalf("unsafe thread returned %v", err)
	}
	clock := time.Unix(1_800_000_000, 0)
	resolver.now = func() time.Time { return clock }
	resolver.cache["expiring"] = cachedProjectURL{url: "https://smith.example/project", expiresAt: clock.Add(time.Second)}
	clock = clock.Add(2 * time.Second)
	if _, ok := resolver.CachedThreadURL("expiring", "thread"); ok {
		t.Fatal("expired project URL remained cached")
	}
}

func TestURLResolverCoalescesLookupsAndHonorsCancellation(t *testing.T) {
	t.Parallel()
	lookup := &projectLookup{url: "https://smith.example/project", started: make(chan struct{}, 1), release: make(chan struct{})}
	resolver := NewURLResolver(lookup, URLResolverOptions{LookupTimeout: time.Second})
	firstDone := make(chan error, 1)
	go func() {
		_, err := resolver.ThreadURL(context.Background(), "project", "one")
		firstDone <- err
	}()
	<-lookup.started
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolver.ThreadURL(cancelled, "project", "two"); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting lookup returned %v", err)
	}
	close(lookup.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if lookup.calls != 1 {
		t.Fatalf("lookup calls = %d", lookup.calls)
	}
}

func TestURLResolverBoundsLookupTime(t *testing.T) {
	t.Parallel()
	lookup := &projectLookup{url: "https://smith.example/project", release: make(chan struct{})}
	resolver := NewURLResolver(lookup, URLResolverOptions{LookupTimeout: time.Millisecond})
	_, err := resolver.ThreadURL(t.Context(), "project", "thread")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lookup timeout returned %v", err)
	}
}

type ignoringLookup struct{ release <-chan struct{} }

func (lookup ignoringLookup) ProjectURL(context.Context, string) (string, error) {
	<-lookup.release
	return "https://smith.example/project", nil
}

func TestURLResolverHardBoundsNoncompliantLookup(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	resolver := NewURLResolver(ignoringLookup{release: release}, URLResolverOptions{LookupTimeout: time.Millisecond, MaxConcurrentLookups: 1})
	started := time.Now()
	_, err := resolver.ThreadURL(t.Context(), "project", "thread")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("hard timeout returned %v after %s", err, time.Since(started))
	}
	close(release)
}

func TestURLResolverRejectsProviderErrorsAndUnsafeURLs(t *testing.T) {
	t.Parallel()
	secret := "provider-secret"
	resolver := NewURLResolver(&projectLookup{err: errors.New(secret)}, URLResolverOptions{})
	_, err := resolver.ThreadURL(t.Context(), "project", "thread")
	if !errors.Is(err, ErrProjectLookup) || strings.Contains(err.Error(), secret) {
		t.Fatalf("lookup error = %v", err)
	}
	resolver = NewURLResolver(&projectLookup{panicValue: secret}, URLResolverOptions{})
	_, err = resolver.ThreadURL(t.Context(), "project", "thread")
	if !errors.Is(err, ErrProjectLookup) || strings.Contains(err.Error(), secret) {
		t.Fatalf("lookup panic = %v", err)
	}
	for _, raw := range []string{"http://smith.example/project", "https://user:" + "secret@smith.example/project", "https://smith.example/project?key=secret"} {
		resolver = NewURLResolver(&projectLookup{url: raw}, URLResolverOptions{})
		if _, err := resolver.ThreadURL(t.Context(), "project", "thread"); !errors.Is(err, ErrProjectURL) {
			t.Fatalf("unsafe URL %q returned %v", raw, err)
		}
	}
}
