//go:build !tinygo

package dago

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dacheckpoint/postgres"
	"github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

// These tests port the whole-agent contracts from the pinned end-to-end,
// snapshot-persistence, and RLM suites.

func TestAgentInterpreterPersistsCheckpointedStateAcrossInvocations(t *testing.T) {
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{
			Check: func(request damodel.Request) error {
				if len(request.Tools) == 0 || !strings.Contains(systemText(request), "persistent QuickJS REPL") {
					return errors.New("interpreter tool or prompt missing")
				}
				return nil
			},
			Response: damodel.Response{Message: interpreterToolCall("eval-1", `const checkpointed = await Promise.resolve(40); checkpointed`)},
		},
		modeltest.Step{
			Check: func(request damodel.Request) error {
				if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "<result>40</result>") {
					return errors.New("first interpreter result missing")
				}
				return nil
			},
			Response: damodel.Response{Message: damessage.Assistant("first done")},
		},
		modeltest.Step{Response: damodel.Response{Message: interpreterToolCall("eval-2", `checkpointed + 2`)}},
		modeltest.Step{
			Check: func(request damodel.Request) error {
				if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "<result>42</result>") {
					return errors.New("checkpointed binding was not restored")
				}
				return nil
			},
			Response: damodel.Response{Message: damessage.Assistant("second done")},
		},
	)
	agent := New(script, Options{
		Interpreter:      Interpreter{Enabled: true, PTC: []string{}},
		Saver:            dacheckpoint.NewMemorySaver(),
		DisableSubagents: true,
		DisableSummary:   true,
	})
	config := dacheckpoint.Config{ThreadID: "interpreter-checkpoint"}
	if _, err := agent.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("first")}}); err != nil {
		t.Fatal(err)
	}
	result, err := agent.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("second")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "second done" || script.Remaining() != 0 {
		t.Fatalf("final response = %q, remaining steps = %d", result.Messages[len(result.Messages)-1].TextContent(), script.Remaining())
	}
}

func TestAgentInterpreterCallsSubagentThroughPTC(t *testing.T) {
	childModel := modeltest.New(damodel.Profile{}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if len(request.Messages) != 1 || request.Messages[0].TextContent() != "child work" {
				return errors.New("subagent input mismatch")
			}
			return nil
		},
		Response: damodel.Response{Message: damessage.Assistant("child result")},
	})
	child := dagent.New(childModel, dagent.Options{})
	parentModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: interpreterToolCall("eval-task", `await tools.task({description:"child work", subagent_type:"special"})`)}},
		modeltest.Step{
			Check: func(request damodel.Request) error {
				last := request.Messages[len(request.Messages)-1]
				if last.Role != damessage.RoleTool || !strings.Contains(last.TextContent(), "child result") {
					return errors.New("PTC subagent result missing")
				}
				return nil
			},
			Response: damodel.Response{Message: damessage.Assistant("parent done")},
		},
	)
	agent := New(parentModel, Options{
		Interpreter:    Interpreter{Enabled: true, PTC: []string{"task"}},
		Subagents:      []Subagent{{Name: "special", Description: "Specialized", Runnable: child}},
		DisableSummary: true,
	})
	result, err := agent.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("delegate")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "parent done" {
		t.Fatalf("final response = %q", result.Messages[len(result.Messages)-1].TextContent())
	}
}

func TestAgentInterpreterSnapshotPersistsThroughSQLite(t *testing.T) {
	saver, err := sqlite.Open(filepath.Join(t.TempDir(), "interpreter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Close()
	assertInterpreterSaverRoundTrip(t, saver, "interpreter-sqlite")
}

func TestAgentInterpreterSnapshotPersistsThroughPostgres(t *testing.T) {
	dsn := os.Getenv("DAGO_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("DAGO_POSTGRES_TEST_DSN is not set")
	}
	saver, err := postgres.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Close()
	thread := "interpreter-postgres-" + time.Now().UTC().Format("20060102150405.000000000")
	if err := saver.DeleteThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = saver.DeleteThread(context.Background(), thread) })
	assertInterpreterSaverRoundTrip(t, saver, thread)
}

func assertInterpreterSaverRoundTrip(t *testing.T, saver dacheckpoint.Saver, thread string) {
	t.Helper()
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: interpreterToolCall("persist-1", `const durableValue = 20; durableValue`)}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("saved")}},
		modeltest.Step{Response: damodel.Response{Message: interpreterToolCall("persist-2", `durableValue * 2`)}},
		modeltest.Step{
			Check: func(request damodel.Request) error {
				if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "<result>40</result>") {
					return errors.New("persisted interpreter value missing")
				}
				return nil
			},
			Response: damodel.Response{Message: damessage.Assistant("restored")},
		},
	)
	agent := New(script, Options{
		Interpreter:      Interpreter{Enabled: true, PTC: []string{}},
		Saver:            saver,
		DisableSubagents: true,
		DisableSummary:   true,
	})
	config := dacheckpoint.Config{ThreadID: thread}
	if _, err := agent.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("save")}}); err != nil {
		t.Fatal(err)
	}
	result, err := agent.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("restore")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "restored" {
		t.Fatalf("final response = %q", result.Messages[len(result.Messages)-1].TextContent())
	}
}

func interpreterToolCall(id, code string) damessage.Message {
	arguments, _ := json.Marshal(map[string]string{"code": code})
	return damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: id, Name: "js_eval", Arguments: arguments}}}
}

func systemText(request damodel.Request) string {
	if request.SystemMessage != nil {
		return request.SystemMessage.TextContent()
	}
	if len(request.Messages) > 0 && request.Messages[0].Role == damessage.RoleSystem {
		return request.Messages[0].TextContent()
	}
	return ""
}
