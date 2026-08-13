package dago

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/datool"
)

type recordingConversationSubagentStore struct {
	conversationID string
	actualSlug     string
	slug           string
	parentID       string
	workingDir     string
}

func (s *recordingConversationSubagentStore) GetOrCreateSubagentConversation(_ context.Context, request ConversationSubagentConversationRequest) (ConversationSubagentConversation, error) {
	s.slug = request.Slug
	s.parentID = request.ParentConversationID
	s.workingDir = request.WorkingDirectory
	return ConversationSubagentConversation{ConversationID: s.conversationID, Slug: s.actualSlug}, nil
}

type recordingConversationSubagentRunner struct {
	conversationID string
	prompt         string
	wait           bool
	timeout        time.Duration
	model          string
	reasoning      string
}

func (r *recordingConversationSubagentRunner) RunSubagent(_ context.Context, request ConversationSubagentRun) (ConversationSubagentReply, error) {
	r.conversationID = request.ConversationID
	r.prompt = request.Prompt
	r.wait = request.Wait
	r.timeout = request.Timeout
	r.model = request.ModelID
	r.reasoning = request.Reasoning
	return ConversationSubagentReply{Content: "finished"}, nil
}

func TestConversationSubagentToolDispatchesPersistentConversation(t *testing.T) {
	store := &recordingConversationSubagentStore{conversationID: "child-1", actualSlug: "research-2"}
	runner := &recordingConversationSubagentRunner{}
	wait := false
	subagentTool := ConversationSubagentTool(store, runner, func() string { return "/workspace" }, ConversationSubagentOptions{
		ParentConversationID: "parent-1",
		ModelID:              "default-model",
		ParentReasoning:      "high",
		AvailableModels: []ConversationSubagentModel{
			{ID: "default-model"},
			{ID: "fast-model", DisplayName: "Fast Model"},
		},
	})

	raw, err := json.Marshal(ConversationSubagentInput{
		Slug:           "Research Task",
		Prompt:         "inspect the API",
		TimeoutSeconds: 7200,
		Wait:           &wait,
		Model:          "fast-model",
		Reasoning:      "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := subagentTool.Execute(context.Background(), raw, datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}

	if store.slug != "research-task" || store.parentID != "parent-1" || store.workingDir != "/workspace" {
		t.Fatalf("unexpected store call: %#v", store)
	}
	if runner.conversationID != "child-1" || runner.prompt != "inspect the API" || runner.wait || runner.timeout != time.Hour || runner.model != "fast-model" || runner.reasoning != "low" {
		t.Fatalf("unexpected runner call: %#v", runner)
	}
	if len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "research-2") || !strings.Contains(result.Content[0].Text, "finished") {
		t.Fatalf("unexpected result content: %#v", result.Content)
	}
	var display ConversationSubagentDisplay
	if err := json.Unmarshal(result.Artifact, &display); err != nil {
		t.Fatal(err)
	}
	if display != (ConversationSubagentDisplay{Slug: "research-2", ConversationID: "child-1"}) {
		t.Fatalf("unexpected display artifact: %#v", display)
	}
}

func TestConversationSubagentToolDefaultsAndContract(t *testing.T) {
	store := &recordingConversationSubagentStore{conversationID: "child-1", actualSlug: "worker"}
	runner := &recordingConversationSubagentRunner{}
	subagentTool := ConversationSubagentTool(store, runner, func() string { return "/workspace" }, ConversationSubagentOptions{
		ParentConversationID: "parent-1",
		ModelID:              "default-model",
		ParentReasoning:      "medium",
		AvailableModels:      []ConversationSubagentModel{{ID: "default-model", DisplayName: "Default Model"}},
	})

	result, err := subagentTool.Execute(context.Background(), json.RawMessage(`{"slug":"worker","prompt":"continue"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !runner.wait || runner.timeout != 15*time.Minute || runner.model != "default-model" || runner.reasoning != "medium" {
		t.Fatalf("defaults not applied: %#v", runner)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "Subagent 'worker' response:\nfinished" {
		t.Fatalf("unexpected result: %#v", result)
	}

	definition := subagentTool.Definition()
	if definition.Name != "subagent" || !strings.Contains(definition.Description, "default-model (Default Model)") {
		t.Fatalf("unexpected definition: %#v", definition)
	}
	if !strings.Contains(string(definition.InputSchema), `"reasoning"`) || !strings.Contains(string(definition.InputSchema), `"default-model"`) {
		t.Fatalf("schema omits configurable fields: %s", definition.InputSchema)
	}
}

func TestConversationSubagentToolCapsLargeTimeoutWithoutOverflow(t *testing.T) {
	store := &recordingConversationSubagentStore{conversationID: "child-1", actualSlug: "worker"}
	runner := &recordingConversationSubagentRunner{}
	subagentTool := ConversationSubagentTool(store, runner, func() string { return "/workspace" }, ConversationSubagentOptions{})
	_, err := subagentTool.Execute(context.Background(), json.RawMessage(`{"slug":"worker","prompt":"work","timeout_seconds":9223372036}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if runner.timeout != time.Hour {
		t.Fatalf("timeout = %v, want %v", runner.timeout, time.Hour)
	}
}

func TestConversationSubagentToolRejectsInvalidOptions(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty slug", input: `{"slug":"","prompt":"work"}`, want: "slug is required"},
		{name: "invalid slug", input: `{"slug":"@#$","prompt":"work"}`, want: "slug must contain alphanumeric characters"},
		{name: "empty prompt", input: `{"slug":"worker","prompt":""}`, want: "prompt is required"},
		{name: "unknown model", input: `{"slug":"worker","prompt":"work","model":"unknown"}`, want: `unknown model "unknown"`},
		{name: "unknown reasoning", input: `{"slug":"worker","prompt":"work","reasoning":"turbo"}`, want: `unknown reasoning level "turbo"`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			subagentTool := ConversationSubagentTool(
				&recordingConversationSubagentStore{},
				&recordingConversationSubagentRunner{},
				func() string { return "/workspace" },
				ConversationSubagentOptions{
					AvailableModels: []ConversationSubagentModel{{ID: "known"}},
				})
			_, err := subagentTool.Execute(context.Background(), json.RawMessage(test.input), datool.Runtime{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestSanitizeSubagentSlug(t *testing.T) {
	cases := map[string]string{
		"Research Task": "research-task",
		"test_slug":     "test-slug",
		"test--slug":    "test-slug",
		"-test-slug-":   "test-slug",
		"test@slug!":    "testslug",
		"@#$":           "",
	}
	for input, want := range cases {
		if got := SanitizeSubagentSlug(input); got != want {
			t.Fatalf("SanitizeSubagentSlug(%q) = %q, want %q", input, got, want)
		}
	}
}
