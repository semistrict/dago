package dacode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dahook"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

func TestHookToolPayloadsShareShapesBoundsAndFailClosedStatus(t *testing.T) {
	call := damessage.ToolCall{ID: "call-1", Name: "read_file", Arguments: []byte(`{"path":"README.md"}`)}
	use := buildHookToolUsePayload(call)
	if use.ToolName != "read_file" || use.ToolID != "call-1" || use.ToolArgs["path"] != "README.md" {
		t.Fatalf("tool use = %#v", use)
	}
	result := datool.TextResult(strings.Repeat("界", hookToolOutputLimit+100))
	result.Status = damessage.ToolStatus("future-status")
	terminal := buildHookToolResultPayload(call, result, nil)
	if terminal.ToolStatus != "error" || len([]rune(terminal.ToolOutput)) != hookToolOutputLimit || !strings.HasSuffix(terminal.ToolOutput, hookToolOutputTruncationMarker) {
		t.Fatalf("bounded terminal payload = status:%q runes:%d suffix:%t", terminal.ToolStatus, len([]rune(terminal.ToolOutput)), strings.HasSuffix(terminal.ToolOutput, hookToolOutputTruncationMarker))
	}
	failure := buildHookToolResultPayload(call, datool.Result{}, errors.New("failed\x00 visibly"))
	if failure.ToolStatus != "error" || failure.ToolOutput != "failed visibly" {
		t.Fatalf("failure payload = %#v", failure)
	}
	errorPayload := buildHookToolErrorPayload(call.Name)
	if len(errorPayload.ToolNames) != 1 || errorPayload.ToolNames[0] != call.Name {
		t.Fatalf("error payload = %#v", errorPayload)
	}
	scalar := call
	scalar.Arguments = []byte(`42`)
	if value := buildHookToolUsePayload(scalar).ToolArgs["value"]; value != float64(42) {
		t.Fatalf("scalar args = %#v", value)
	}
	malformed := call
	malformed.Arguments = []byte(`{"path"`)
	if arguments := buildHookToolUsePayload(malformed).ToolArgs; len(arguments) != 0 {
		t.Fatalf("malformed args were not fail closed: %#v", arguments)
	}
}

func TestHookUISinkUsesNewestActiveStatusAndNeverBlocksPublisher(t *testing.T) {
	sink := newHookUISink()
	sink.Publish(dahook.Progress{OperationID: "first", Event: dahook.PreToolUse, Active: true})
	sink.Publish(dahook.Progress{OperationID: "second", Event: dahook.PostToolUse, Active: true, Message: "Checking output"})
	update, err := sink.Next(t.Context())
	if err != nil || update.Status != "Checking output" || update.Event != dahook.PostToolUse || !update.Active {
		t.Fatalf("latest status = %#v, %v", update, err)
	}
	sink.Publish(dahook.Progress{OperationID: "second", Event: dahook.PostToolUse})
	update, err = sink.Next(t.Context())
	if err != nil || update.Status != "Running PreToolUse hook..." || update.Event != dahook.PreToolUse || !update.Active {
		t.Fatalf("restored status = %#v, %v", update, err)
	}
	sink.Publish(dahook.Progress{OperationID: "first", Event: dahook.PreToolUse})
	update, err = sink.Next(t.Context())
	if err != nil || update.Status != "" || update.Active {
		t.Fatalf("cleared status = %#v, %v", update, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sink.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	started := time.Now()
	for index := 0; index < 10_000; index++ {
		sink.Publish(dahook.Progress{OperationID: "busy", Event: dahook.Stop, Active: index%2 == 0})
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("coalesced publisher blocked for %s", elapsed)
	}
}
