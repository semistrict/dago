package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

func TestUseSimplifiedPatch(t *testing.T) {
	tests := []struct {
		name    string
		profile damodel.Profile
		want    bool
	}{
		{
			name: "standard patch profile",
		},
		{
			name:    "simplified patch profile",
			profile: damodel.Profile{UseSimplifiedPatch: true},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.profile.UseSimplifiedPatch != tt.want {
				t.Errorf("UseSimplifiedPatch = %v, want %v", tt.profile.UseSimplifiedPatch, tt.want)
			}
		})
	}
}

func TestTextContent(t *testing.T) {
	text := "test text content"
	contents := TextContent(text)

	if len(contents) != 1 {
		t.Errorf("TextContent() returned %d items, want 1", len(contents))
	}

	if contents[0].Type != ContentTypeText {
		t.Errorf("TextContent()[0].Type = %v, want %v", contents[0].Type, ContentTypeText)
	}

	if contents[0].Text != text {
		t.Errorf("TextContent()[0].Text = %s, want %s", contents[0].Text, text)
	}
}

func TestErrorToolOut(t *testing.T) {
	want := fmt.Errorf("test error")
	native := datool.Func{
		Spec: datool.Definition{Name: "fail", Description: "fail", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			return datool.Result{}, want
		},
	}
	_, err := native.Execute(context.Background(), json.RawMessage(`{}`), datool.Runtime{})
	if !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("native tool error = %v, want wrapped %v", err, want)
	}
}

func TestErrorfToolOut(t *testing.T) {
	format := "error: %s"
	arg := "test"
	expected := fmt.Sprintf(format, arg)
	native := datool.Func{
		Spec: datool.Definition{Name: "fail", Description: "fail", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			return datool.Result{}, fmt.Errorf(format, arg)
		},
	}
	_, err := native.Execute(context.Background(), json.RawMessage(`{}`), datool.Runtime{})
	if err == nil || !strings.Contains(err.Error(), expected) {
		t.Fatalf("native tool error = %v, want %q", err, expected)
	}
}

func TestRunJSON(t *testing.T) {
	type request struct {
		Name string `json:"name"`
	}

	var gotCtx context.Context
	var gotReq request
	native := datool.Func{
		Spec: datool.Definition{
			Name: "greet", Description: "greet a person",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
		},
		Run: func(ctx context.Context, raw json.RawMessage, _ datool.Runtime) (datool.Result, error) {
			var req request
			if err := json.Unmarshal(raw, &req); err != nil {
				return datool.Result{}, fmt.Errorf("invalid tool input: %w", err)
			}
			gotCtx = ctx
			gotReq = req
			return datool.TextResult("hello " + req.Name), nil
		},
	}

	ctx := context.WithValue(context.Background(), struct{}{}, "ctx-value")
	out, err := native.Execute(ctx, json.RawMessage(`{"name":"Ada"}`), datool.Runtime{})
	if err != nil {
		t.Fatalf("native tool returned error: %v", err)
	}
	if gotCtx != ctx {
		t.Fatal("RunJSON did not pass through context")
	}
	if gotReq.Name != "Ada" {
		t.Fatalf("RunJSON decoded request %+v, want name Ada", gotReq)
	}
	if len(out.Content) != 1 || out.Content[0].Type != dmessage.BlockText || out.Content[0].Text != "hello Ada" {
		t.Fatalf("native tool output = %+v", out.Content)
	}
}

func TestRunJSONInvalidJSON(t *testing.T) {
	type request struct {
		Name string `json:"name"`
	}

	called := false
	native := datool.Func{
		Spec: datool.Definition{Name: "greet", Description: "greet", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		Run: func(_ context.Context, raw json.RawMessage, _ datool.Runtime) (datool.Result, error) {
			var req request
			if err := json.Unmarshal(raw, &req); err != nil {
				return datool.Result{}, fmt.Errorf("invalid tool input: %w", err)
			}
			called = true
			return datool.Result{}, nil
		},
	}

	_, err := native.Execute(context.Background(), json.RawMessage(`{"name":123}`), datool.Runtime{})
	if err == nil {
		t.Fatal("native tool returned nil error for invalid input")
	}
	if called {
		t.Fatal("RunJSON called handler after invalid input")
	}
}

func TestUsageAdd(t *testing.T) {
	u1 := Usage{
		InputTokens:              100,
		CacheCreationInputTokens: 50,
		CacheReadInputTokens:     25,
		OutputTokens:             200,
		CostUSD:                  0.01,
	}

	u2 := Usage{
		InputTokens:              150,
		CacheCreationInputTokens: 75,
		CacheReadInputTokens:     30,
		OutputTokens:             100,
		CostUSD:                  0.02,
	}

	u1.Add(u2)

	expected := Usage{
		InputTokens:              250,  // 100 + 150
		CacheCreationInputTokens: 125,  // 50 + 75
		CacheReadInputTokens:     55,   // 25 + 30
		OutputTokens:             300,  // 200 + 100
		CostUSD:                  0.03, // 0.01 + 0.02
	}

	if u1 != expected {
		t.Errorf("Usage.Add() resulted in %v, want %v", u1, expected)
	}
}

func TestUsageString(t *testing.T) {
	tests := []struct {
		name  string
		usage Usage
		want  string
	}{
		{
			name: "normal usage",
			usage: Usage{
				InputTokens:  100,
				OutputTokens: 50,
			},
			want: "in: 100, out: 50",
		},
		{
			name: "zero usage",
			usage: Usage{
				InputTokens:  0,
				OutputTokens: 0,
			},
			want: "in: 0, out: 0",
		},
		{
			name: "high usage",
			usage: Usage{
				InputTokens:  1000000,
				OutputTokens: 500000,
			},
			want: "in: 1000000, out: 500000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.usage.String()
			if result != tt.want {
				t.Errorf("Usage.String() = %s, want %s", result, tt.want)
			}
		})
	}
}

func TestUsageIsZero(t *testing.T) {
	tests := []struct {
		name  string
		usage Usage
		want  bool
	}{
		{
			name:  "zero usage",
			usage: Usage{},
			want:  true,
		},
		{
			name: "non-zero input tokens",
			usage: Usage{
				InputTokens: 1,
			},
			want: false,
		},
		{
			name: "non-zero output tokens",
			usage: Usage{
				OutputTokens: 1,
			},
			want: false,
		},
		{
			name: "non-zero cost",
			usage: Usage{
				CostUSD: 0.01,
			},
			want: false,
		},
		{
			name: "all fields zero",
			usage: Usage{
				InputTokens:              0,
				CacheCreationInputTokens: 0,
				CacheReadInputTokens:     0,
				OutputTokens:             0,
				CostUSD:                  0,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.usage.IsZero()
			if result != tt.want {
				t.Errorf("Usage.IsZero() = %v, want %v", result, tt.want)
			}
		})
	}
}

func TestResponseToMessage(t *testing.T) {
	tests := []struct {
		name          string
		response      Response
		wantRole      MessageRole
		wantEndOfTurn bool
	}{
		{
			name: "tool use stop reason",
			response: Response{
				Role:       MessageRoleAssistant,
				StopReason: StopReasonToolUse,
			},
			wantRole:      MessageRoleAssistant,
			wantEndOfTurn: false,
		},
		{
			name: "end turn stop reason",
			response: Response{
				Role:       MessageRoleAssistant,
				StopReason: StopReasonEndTurn,
			},
			wantRole:      MessageRoleAssistant,
			wantEndOfTurn: true,
		},
		{
			name: "max tokens stop reason",
			response: Response{
				Role:       MessageRoleAssistant,
				StopReason: StopReasonMaxTokens,
			},
			wantRole:      MessageRoleAssistant,
			wantEndOfTurn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := tt.response.ToMessage()

			if message.Role != tt.wantRole {
				t.Errorf("ToMessage().Role = %v, want %v", message.Role, tt.wantRole)
			}

			if message.EndOfTurn != tt.wantEndOfTurn {
				t.Errorf("ToMessage().EndOfTurn = %v, want %v", message.EndOfTurn, tt.wantEndOfTurn)
			}
		})
	}
}

func TestUsageAttr(t *testing.T) {
	usage := Usage{
		InputTokens:              100,
		OutputTokens:             50,
		CacheCreationInputTokens: 25,
		CacheReadInputTokens:     75,
		CostUSD:                  0.01,
	}

	attr := usage.Attr()
	if attr.Key != "usage" {
		t.Errorf("Attr().Key = %s, want 'usage'", attr.Key)
	}
}

func TestFormatRetryEvent(t *testing.T) {
	msg := FormatRetryEvent(RetryEvent{
		Sleep:    16 * time.Second,
		Err:      `transport: Post "http://169.254.169.254/gateway/llm/_/gateway/anthropic/v1/messages": dial tcp 169.254.169.254:80: i/o timeout`,
		Provider: "anthropic",
		Model:    "claude-opus-4-7",
	})

	want := `LLM request failed: anthropic claude-opus-4-7; retrying in 16s. transport: Post "http://169.254.169.254/gateway/llm/_/gateway/anthropic/v1/messages": dial tcp 169.254.169.254:80: i/o timeout`
	if msg != want {
		t.Fatalf("FormatRetryEvent() = %q, want %q", msg, want)
	}
}

// TestContentCallerCitationsOmitEmpty pins the wire-format-safety contract of
// llm.Content.Caller and llm.Content.Citations.
//
// These fields are persisted to the messages table as part of llm_data JSON
// and later reloaded to be sent back to the LLM. Without `omitempty`, a nil
// json.RawMessage marshals to the JSON token `null`, which on reload
// unmarshals back to []byte("null") (not nil). We would then forward
// `"caller": null` to Anthropic, which the API rejects with
//
//	server_tool_use.caller: Input should be an object
//
// and the bad block sits in conversation history forever, wedging every
// retry. With omitempty, nil values are dropped on marshal and never come
// back to bite us.
func TestContentCallerCitationsOmitEmpty(t *testing.T) {
	original := Content{
		Type:     ContentTypeServerToolUse,
		ID:       "srvtoolu_x",
		ToolName: "web_search",
	}
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, `"Caller"`) || strings.Contains(s, `"Citations"`) {
		t.Fatalf("nil Caller/Citations must not appear in marshaled JSON; got: %s", s)
	}
	var reloaded Content
	if err := json.Unmarshal(b, &reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.Caller != nil {
		t.Errorf("reloaded Caller = %q, want nil", reloaded.Caller)
	}
	if reloaded.Citations != nil {
		t.Errorf("reloaded Citations = %q, want nil", reloaded.Citations)
	}
}
