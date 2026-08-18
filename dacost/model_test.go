package dacost

import (
	"context"
	"io"
	"testing"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/datool"
)

func TestNormalizeUsageRejectsTypedNilModel(t *testing.T) {
	var model *modeltest.Scripted
	defer func() {
		if recover() == nil {
			t.Fatal("typed-nil model did not panic")
		}
	}()
	NormalizeUsage(model)
}

func TestNormalizeUsageFillsInvokeFallbackWithoutMutatingProviderResponse(t *testing.T) {
	original := damessage.Usage{InputTokens: 10, OutputTokens: 2}
	inner := modeltest.New(damodel.Profile{Provider: "test", Model: "fallback"}, modeltest.Step{Response: damodel.Response{Message: damessage.Message{
		Role: damessage.RoleAssistant, Usage: &original,
	}}})
	response, err := NormalizeUsage(inner).Invoke(t.Context(), damodel.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Usage.Provider != "test" || response.Message.Usage.Model != "fallback" {
		t.Fatalf("usage = %#v", response.Message.Usage)
	}
	if original.Provider != "" || original.Model != "" {
		t.Fatalf("provider response was mutated: %#v", original)
	}
}

func TestNormalizeUsageAccumulatesStreamCorrectionsAndLateModel(t *testing.T) {
	inner := modeltest.New(damodel.Profile{Provider: "google", Model: "fallback", NativeStreaming: true}, modeltest.Step{Chunks: []damodel.Chunk{
		{MessageDelta: damessage.Message{Usage: &damessage.Usage{InputTokens: 100, OutputTokens: 2, TotalTokens: 102, InputDetails: map[string]int{"cache_read": 20}}}},
		{MessageDelta: damessage.Message{Usage: &damessage.Usage{InputTokens: -10, OutputTokens: 3, TotalTokens: -7, Model: "actual", InputDetails: map[string]int{"cache_read": -5}}}, Done: true},
	}})
	stream, err := NormalizeUsage(inner).Stream(t.Context(), damodel.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.MessageDelta.Usage.InputTokens != 100 || first.MessageDelta.Usage.Model != "fallback" {
		t.Fatalf("first usage = %#v", first.MessageDelta.Usage)
	}
	final, err := stream.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	usage := final.MessageDelta.Usage
	if usage.InputTokens != 90 || usage.OutputTokens != 5 || usage.TotalTokens != 95 || usage.Model != "actual" || usage.Provider != "google" || usage.InputDetails["cache_read"] != 15 {
		t.Fatalf("final usage = %#v", usage)
	}
	if _, err := stream.Next(t.Context()); err != io.EOF {
		t.Fatalf("stream end = %v", err)
	}
}

func TestNormalizeUsagePreservesOptionalModelCapabilities(t *testing.T) {
	plain := NormalizeUsage(modeltest.New(damodel.Profile{}, modeltest.Step{}))
	if _, ok := plain.(damodel.Binder); ok {
		t.Fatal("plain model unexpectedly implements Binder")
	}
	if _, ok := plain.(damodel.TokenCounter); ok {
		t.Fatal("plain model unexpectedly implements TokenCounter")
	}
	inner := capabilityModel{Chat: modeltest.New(damodel.Profile{Provider: "test", Model: "m"}, modeltest.Step{})}
	wrapped := NormalizeUsage(inner)
	if _, ok := wrapped.(damodel.Binder); !ok {
		t.Fatal("Binder capability was lost")
	}
	counter, ok := wrapped.(damodel.TokenCounter)
	if !ok {
		t.Fatal("TokenCounter capability was lost")
	}
	if count, err := counter.CountTokens(t.Context(), nil); err != nil || count != 7 {
		t.Fatalf("CountTokens() = %d, %v", count, err)
	}
	bound, err := wrapped.(damodel.Binder).BindTools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bound.(damodel.Binder); !ok {
		t.Fatal("bound model lost Binder capability")
	}
}

type capabilityModel struct{ damodel.Chat }

func (model capabilityModel) BindTools([]datool.Definition) (damodel.Chat, error)     { return model, nil }
func (capabilityModel) CountTokens(context.Context, []damessage.Message) (int, error) { return 7, nil }
