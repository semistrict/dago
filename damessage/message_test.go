package damessage

import (
	"strings"
	"testing"
)

func TestMessageValidate(t *testing.T) {
	tests := []struct {
		name    string
		message Message
		want    string
	}{
		{name: "human", message: Human("hello")},
		{name: "tool missing id", message: Text(RoleTool, "result"), want: "tool call id"},
		{name: "remove missing id", message: Message{Role: RoleRemove}, want: "requires an id"},
		{
			name: "invalid tool arguments",
			message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{
				ID: "call", Name: "lookup", Arguments: []byte("{"),
			}}},
			want: "invalid JSON",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.message.Validate()
			if test.want == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestMessageFrom(t *testing.T) {
	original := Assistant("done")
	tests := []struct {
		name  string
		value any
		role  Role
		text  string
	}{
		{name: "message", value: original, role: RoleAssistant, text: "done"},
		{name: "message pointer", value: &original, role: RoleAssistant, text: "done"},
		{name: "string", value: "hello", role: RoleHuman, text: "hello"},
		{name: "object", value: struct {
			Question string `json:"question"`
		}{Question: "hello"}, role: RoleHuman, text: `{"question":"hello"}`},
		{name: "nil message pointer", value: (*Message)(nil), role: RoleHuman, text: "null"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, err := MessageFrom(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if message.Role != test.role || message.TextContent() != test.text {
				t.Fatalf("MessageFrom() = %#v", message)
			}
		})
	}
}

func TestMessageFromRejectsNonJSONValue(t *testing.T) {
	if _, err := MessageFrom(func() {}); err == nil || !strings.Contains(err.Error(), "convert message to JSON") {
		t.Fatalf("MessageFrom() error = %v", err)
	}
}

func TestApproximateTokensIsStableAndNonzero(t *testing.T) {
	messages := []Message{
		System("You are helpful."),
		Human("Hello"),
		{
			Role: RoleAssistant,
			ToolCalls: []ToolCall{{
				ID: "call", Name: "lookup", Arguments: []byte(`{"query":"go"}`),
			}},
		},
	}
	first := ApproximateTokens(messages)
	second := ApproximateTokens(messages)
	if first <= 0 || first != second {
		t.Fatalf("ApproximateTokens() = %d, then %d", first, second)
	}
}
