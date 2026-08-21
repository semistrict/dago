package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

func main() {
	if len(os.Args) < 3 || len(os.Args) > 4 {
		fmt.Fprintln(os.Stderr, "usage: sessionseed DATABASE DIRECTORY [hostile|virtualized|lifecycle|offload|transparent]")
		os.Exit(2)
	}
	mode := ""
	if len(os.Args) == 4 {
		mode = os.Args[3]
	}
	if mode != "" && mode != "hostile" && mode != "virtualized" && mode != "lifecycle" && mode != "offload" && mode != "transparent" {
		fmt.Fprintln(os.Stderr, "mode must be hostile, virtualized, lifecycle, offload, or transparent")
		os.Exit(2)
	}
	if err := seed(os.Args[1], os.Args[2], mode); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func seed(databasePath, directory, mode string) error {
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		return err
	}
	saver, err := checkpointsqlite.Open(databasePath)
	if err != nil {
		return err
	}
	defer saver.Close()
	agent := dagent.New(modeltest.New(damodel.Profile{}), dagent.Options{Saver: saver})
	for _, session := range []struct {
		id, prompt, answer string
	}{
		{id: "playwright-older", prompt: "Older browser task", answer: "Older browser answer"},
		{id: "playwright-newer", prompt: "Newer browser task", answer: "Newer browser answer"},
	} {
		_, err := agent.UpdateState(context.Background(), dacheckpoint.Config{ThreadID: session.id}, dastate.Values{
			dagent.MessagesKey:           []damessage.Message{damessage.Human(session.prompt), damessage.Assistant(session.answer)},
			"__dacode_working_directory": directory,
		})
		if err != nil {
			return err
		}
	}
	if mode == "virtualized" {
		messages := make([]damessage.Message, 0, 190)
		for index := range 95 {
			messages = append(messages,
				damessage.Human(fmt.Sprintf("Virtual history user %03d", index)),
				damessage.Assistant(fmt.Sprintf("Virtual history assistant %03d", index)),
			)
		}
		_, err = agent.UpdateState(context.Background(), dacheckpoint.Config{ThreadID: "playwright-virtualized"}, dastate.Values{
			dagent.MessagesKey: messages, "__dacode_working_directory": directory,
		})
		return err
	}
	if mode == "transparent" {
		assistant := damessage.Message{
			Role: damessage.RoleAssistant,
			ToolCalls: []damessage.ToolCall{{
				ID: "eval-call", Name: "js_eval", Arguments: json.RawMessage(`{"code":"await tools.readFile({file_path:'/guide.md'})"}`),
			}},
			Metadata: map[string]json.RawMessage{},
		}
		if err := damessage.SetMetadata(assistant.Metadata, datool.PTCTransparencyMetadataKey, datool.PTCTransparencyMetadata{ParentCallIDs: []string{"eval-call"}}); err != nil {
			return err
		}
		artifact, err := json.Marshal(datool.PTCTranscriptArtifact{
			Type: datool.PTCTranscriptArtifactType,
			Calls: []datool.PTCTranscriptCall{{
				CallID: "ptc-read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/guide.md"}`),
				Output: "restored guide contents", Status: damessage.ToolStatusSuccess,
			}},
		})
		if err != nil {
			return err
		}
		result := damessage.Tool("eval-call", "restored guide contents")
		result.Name = "js_eval"
		result.Artifact = artifact
		_, err = agent.UpdateState(context.Background(), dacheckpoint.Config{ThreadID: "playwright-transparent"}, dastate.Values{
			dagent.MessagesKey: []damessage.Message{
				damessage.Human("Read the guide"), assistant, result, damessage.Assistant("Guide read complete."),
			},
			"__dacode_working_directory": directory,
		})
		return err
	}
	if mode == "lifecycle" || mode == "offload" {
		messages := make([]damessage.Message, 0, 80)
		for index := range 40 {
			messages = append(messages,
				damessage.Human(fmt.Sprintf("Compaction history user %03d %s", index, strings.Repeat("context ", 160))),
				damessage.Assistant(fmt.Sprintf("Compaction history assistant %03d %s", index, strings.Repeat("detail ", 160))),
			)
		}
		values := dastate.Values{
			dagent.MessagesKey: messages, "__dacode_working_directory": directory,
		}
		threadID := "playwright-offload"
		if mode == "lifecycle" {
			threadID = "playwright-lifecycle"
			messages[len(messages)-1].Usage = &damessage.Usage{InputTokens: 400_000, OutputTokens: 1_000, TotalTokens: 401_000}
			values[dagent.MessagesKey] = messages
			values["__dacode_agent_name"] = "builder"
			values["__dacode_agent_generation"] = "0123456789abcdef01234567"
			values["_context_tokens"] = 401_000
		}
		_, err = agent.UpdateState(context.Background(), dacheckpoint.Config{ThreadID: threadID}, values)
		return err
	}
	if mode != "hostile" {
		return nil
	}
	osc52 := func(text string) string {
		return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
	}
	oscOpenURL := "\x1b]777;dago-open-url;" + base64.StdEncoding.EncodeToString([]byte("https://github.com/semistrict/dago/issues/new")) + "\x07"
	assistant := damessage.Assistant("Assistant before " + osc52("assistant-overwrite") + oscOpenURL + " after\nAssistant second line")
	assistant.ToolCalls = []damessage.ToolCall{{ID: "hostile-call", Name: "hostile_tool"}}
	_, err = agent.UpdateState(context.Background(), dacheckpoint.Config{ThreadID: "playwright-hostile"}, dastate.Values{
		dagent.MessagesKey: []damessage.Message{
			damessage.Human("Hostile terminal output"),
			assistant,
			damessage.Tool("hostile-call", "Tool before "+osc52("tool-overwrite")+oscOpenURL+" after\nTool second line"),
			damessage.Assistant("Latest safe assistant"),
		},
		"__dacode_working_directory": directory,
	})
	if err != nil {
		return err
	}
	return nil
}
