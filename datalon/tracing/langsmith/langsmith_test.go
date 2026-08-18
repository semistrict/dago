package langsmith

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	ls "github.com/langchain-ai/langsmith-go"
	"github.com/semistrict/dago/datalon/tracing"
)

func TestSinkMapsRunAndCompletion(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	sink := New(client)
	start := time.Unix(1_800_000_000, 123_456_000).UTC()
	span, err := sink.Begin(t.Context(), tracing.Run{Name: "talon.agent", Project: "project", Input: "hello", Metadata: map[string]any{"trigger": "cron"}, Tags: []string{"tag"}, StartTime: start})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.created) != 1 {
		t.Fatalf("creates = %d", len(client.created))
	}
	created := client.created[0]
	if created.ID == [16]byte{} || created.TraceID != created.ID || created.RunType != "chain" || created.SessionName != "project" || created.Inputs["input"] != "hello" {
		t.Fatalf("create = %#v", created)
	}
	if !strings.Contains(created.DottedOrder, created.ID.String()) || !strings.HasPrefix(created.DottedOrder, "20270115T080000123456Z") {
		t.Fatalf("dotted = %q", created.DottedOrder)
	}
	end := start.Add(time.Second)
	if err := span.End(t.Context(), tracing.Completion{Output: "answer", EndTime: end}); err != nil {
		t.Fatal(err)
	}
	if len(client.updated) != 1 || client.updated[0].ID != created.ID || client.updated[0].Outputs["output"] != "answer" || !client.updated[0].EndTime.Equal(end) {
		t.Fatalf("update = %#v", client.updated)
	}
}

func TestSinkMapsErrorsAndPreservesClientFailures(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	sink := New(client)
	span, err := sink.Begin(t.Context(), tracing.Run{Name: "run", Project: "project", StartTime: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	client.updateErr = errors.New("update failed")
	err = span.End(t.Context(), tracing.Completion{Error: "runtime failed", EndTime: time.Now()})
	if !errors.Is(err, client.updateErr) || client.updated[0].Error != "runtime failed" || client.updated[0].Outputs != nil {
		t.Fatalf("error = %v, update = %#v", err, client.updated[0])
	}
	client.createErr = errors.New("create failed")
	if _, err := sink.Begin(t.Context(), tracing.Run{StartTime: time.Now()}); !errors.Is(err, client.createErr) {
		t.Fatalf("create error = %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sink.Begin(cancelled, tracing.Run{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

type fakeClientFactory struct {
	client           Client
	endpoint, apiKey string
	err              error
}

func (factory *fakeClientFactory) NewTracingClient(ctx context.Context, endpoint, apiKey string) (Client, error) {
	factory.endpoint, factory.apiKey = endpoint, apiKey
	return factory.client, factory.err
}

func TestFactoryBridgesResolvedCredentials(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	clients := &fakeClientFactory{client: client}
	sink, err := NewFactory(clients).NewTracingSink(t.Context(), "https://eu.api.smith.langchain.com", "secret")
	if err != nil || sink == nil || clients.endpoint != "https://eu.api.smith.langchain.com" || clients.apiKey != "secret" {
		t.Fatalf("factory bridge mismatch: sink=%T err=%v endpoint=%q", sink, err, clients.endpoint)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewFactory(clients).NewTracingSink(cancelled, "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled factory returned %v", err)
	}
	clients.err = errors.New("secret provider detail")
	if _, err := NewFactory(clients).NewTracingSink(t.Context(), "", "secret"); !errors.Is(err, ErrClientFactory) || strings.Contains(err.Error(), "secret provider detail") {
		t.Fatalf("client factory error = %v", err)
	}
}

type fakeClient struct {
	created              []*ls.RunCreate
	updated              []*ls.RunUpdate
	createErr, updateErr error
}

func (client *fakeClient) CreateRun(run *ls.RunCreate) error {
	copy := *run
	client.created = append(client.created, &copy)
	return client.createErr
}
func (client *fakeClient) UpdateRun(run *ls.RunUpdate) error {
	copy := *run
	client.updated = append(client.updated, &copy)
	return client.updateErr
}
