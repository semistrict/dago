package dago

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/tool"
)

func TestNemotronToolCallShimRepairsArgumentsAndEmptyResult(t *testing.T) {
	middleware := NemotronToolCallShim()
	request := agent.ToolCallRequest{Call: message.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"/big.txt"}`)}}
	var captured agent.ToolCallRequest
	response, err := middleware.WrapToolCall(context.Background(), request, func(_ context.Context, request agent.ToolCallRequest) (agent.ToolCallResponse, error) {
		captured = request
		return agent.ToolCallResponse{Result: tool.Result{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var arguments map[string]any
	if err := json.Unmarshal(captured.Call.Arguments, &arguments); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"file_path": "/big.txt", "limit": float64(nemotronDefaultReadLimit)}
	if !reflect.DeepEqual(arguments, want) {
		t.Fatalf("arguments = %#v, want %#v", arguments, want)
	}
	if resultText(response.Result.Content) != nemotronEmptyToolResult {
		t.Fatalf("result = %#v", response.Result.Content)
	}
}

func TestNemotronToolCallShimPreservesExplicitPathAndCommandResult(t *testing.T) {
	middleware := NemotronToolCallShim()
	request := agent.ToolCallRequest{Call: message.ToolCall{ID: "call-1", Name: "delete", Arguments: json.RawMessage(`{"path":"/wrong","file_path":"/right"}`)}}
	response, err := middleware.WrapToolCall(context.Background(), request, func(_ context.Context, request agent.ToolCallRequest) (agent.ToolCallResponse, error) {
		if strings.Contains(string(request.Call.Arguments), `"file_path":"/wrong"`) {
			t.Fatal("path overwrote file_path")
		}
		return agent.ToolCallResponse{Result: tool.Result{Update: map[string]any{"changed": true}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Result.Content) != 0 {
		t.Fatalf("state update received placeholder: %#v", response.Result)
	}
}

func TestNemotronReadContinuationNotice(t *testing.T) {
	middleware := NemotronReadContinuationNotice()
	request := agent.ToolCallRequest{Call: message.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/x","offset":9,"limit":3}`)}}
	response, err := middleware.WrapToolCall(context.Background(), request, func(context.Context, agent.ToolCallRequest) (agent.ToolCallResponse, error) {
		return agent.ToolCallResponse{Result: tool.TextResult("1  alpha\n2  beta\n3  gamma")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(response.Result.Content)
	if !strings.Contains(text, "3 lines starting at offset 9") || !strings.Contains(text, "offset=12") {
		t.Fatalf("result = %q", text)
	}
}

func TestNemotronReadContinuationNoticeIgnoresWrappedRows(t *testing.T) {
	for input, want := range map[string]bool{
		"1  source": true, " 10  source": true, "1\tsource": true,
		"1 source": false, "1.1  wrapped": false, " 5.1  wrapped": false, "plain": false,
	} {
		if got := isNumberedReadRow(input); got != want {
			t.Errorf("isNumberedReadRow(%q) = %v, want %v", input, got, want)
		}
	}
	middleware := NemotronReadContinuationNotice()
	request := agent.ToolCallRequest{Call: message.ToolCall{Name: "read_file", Arguments: json.RawMessage(`{"limit":2}`)}}
	response, err := middleware.WrapToolCall(context.Background(), request, func(context.Context, agent.ToolCallRequest) (agent.ToolCallResponse, error) {
		return agent.ToolCallResponse{Result: tool.TextResult("  1  first\n1.1  second\n1.2  third")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resultText(response.Result.Content), "per-read limit") {
		t.Fatalf("wrapped rows counted: %#v", response.Result.Content)
	}
}

func TestNemotronModelRateLimitRetry(t *testing.T) {
	middleware := NemotronModelRateLimitRetry(0)
	calls := 0
	response, err := middleware.WrapModelCall(context.Background(), agent.ModelRequest{}, func(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
		calls++
		if calls == 1 {
			return agent.ModelResponse{}, retryStatusError{status: 429}
		}
		return agent.ModelResponse{Messages: []message.Message{message.Assistant("ok")}}, nil
	})
	if err != nil || calls != 2 || response.Messages[0].TextContent() != "ok" {
		t.Fatalf("calls = %d, response = %#v, error = %v", calls, response, err)
	}

	calls = 0
	_, err = middleware.WrapModelCall(context.Background(), agent.ModelRequest{}, func(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
		calls++
		return agent.ModelResponse{}, errors.New("invalid request")
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls = %d, error = %v", calls, err)
	}
}

func TestNemotronModelRateLimitRetryHonorsCancellation(t *testing.T) {
	middleware := NemotronModelRateLimitRetry(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := middleware.WrapModelCall(ctx, agent.ModelRequest{}, func(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
		return agent.ModelResponse{}, retryStatusError{status: 429}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestNemotronReasoningTagCleanup(t *testing.T) {
	middleware := NemotronReasoningTagCleanup()
	response, err := middleware.WrapModelCall(context.Background(), agent.ModelRequest{}, func(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
		return agent.ModelResponse{Messages: []message.Message{message.Assistant("<think>hidden reasoning</think>\nVisible answer")}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	content := response.Messages[0].Content
	if len(content) != 2 || content[0].Type != message.BlockReasoning || content[0].Reasoning != "hidden reasoning" || content[1].Text != "Visible answer" {
		t.Fatalf("content = %#v", content)
	}
}

func TestNemotronTextToolCallParserRepairsSupportedFormats(t *testing.T) {
	available := map[string]bool{"grep": true, "execute": true, "get_service_name": true}
	tests := []struct {
		text string
		name string
		args string
		left string
	}{
		{text: "Run this.\n<function=grep><parameter name=pattern>MAGIC</parameter><parameter name=path>/workspace</parameter></function>", name: "grep", args: `{"path":"/workspace","pattern":"MAGIC"}`, left: "Run this."},
		{text: `{"tool":"bash","cmd":"pytest -q"}`, name: "execute", args: `{"command":"pytest -q"}`},
		{text: "<function>\n<name=get_service_name</name>\n<parameter>\n<service_id>:0\n</parameter>\n</function>\n</tool_call>", name: "get_service_name", args: `{"service_id":"0"}`},
	}
	for _, test := range tests {
		got := repairNemotronTextToolCalls(message.Assistant(test.text), available)
		if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != test.name || string(got.ToolCalls[0].Arguments) != test.args || got.TextContent() != test.left {
			t.Errorf("repair %q = %#v", test.text, got)
		}
	}
}

func TestNemotronTextToolCallParserRejectsUnavailableTools(t *testing.T) {
	original := message.Assistant("<function=execute><parameter name=command>pytest -q</parameter></function>")
	got := repairNemotronTextToolCalls(original, map[string]bool{"read_file": true})
	if len(got.ToolCalls) != 0 || got.TextContent() != original.TextContent() {
		t.Fatalf("message = %#v", got)
	}
}

func TestNemotronFilesystemRetryIsScoped(t *testing.T) {
	middleware := nemotronFilesystemRetry()
	for _, test := range []struct {
		name string
		want int
	}{{name: "read_file", want: 2}, {name: "send_email", want: 1}} {
		calls := 0
		_, err := middleware.WrapToolCall(context.Background(), agent.ToolCallRequest{Call: message.ToolCall{Name: test.name}}, func(context.Context, agent.ToolCallRequest) (agent.ToolCallResponse, error) {
			calls++
			return agent.ToolCallResponse{}, errors.New("failed")
		})
		if err == nil || calls != test.want {
			t.Errorf("%s calls = %d, error = %v", test.name, calls, err)
		}
	}
}

type retryStatusError struct{ status int }

func (err retryStatusError) Error() string { return "throttled" }
func (err retryStatusError) RetryEvent(_ int, _ time.Duration) model.RetryEvent {
	return model.RetryEvent{Status: err.status}
}
