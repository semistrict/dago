package dago

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/checkpoint"
	checkpointsqlite "github.com/semistrict/dago/checkpoint/sqlite"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
)

func TestHumanMessageEvictionSurvivesSQLiteReplayWithoutDuplicates(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "checkpoints.sqlite")
	limit := 4
	lines := make([]string, 20)
	for index := range lines {
		lines[index] = fmt.Sprintf("line-%02d-%s", index+1, strings.Repeat("x", 40))
	}
	firstText := strings.Join(lines[:10], "\n")
	secondText := strings.Join(lines[10:], "\n")
	fullText := firstText + "\n" + secondText
	image := message.ContentBlock{Type: message.BlockImage, MIMEType: "image/png", Data: []byte{1, 2, 3}}

	firstModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		human := onlyRole(request.Messages, message.RoleHuman)
		if len(human) != 1 {
			return fmt.Errorf("human messages = %#v", human)
		}
		if text := human[0].TextContent(); !strings.Contains(text, "Message content too large") || !strings.Contains(text, "10 lines truncated") || strings.Contains(text, "line-10-") {
			return fmt.Errorf("model-facing human content = %q", text)
		}
		if len(human[0].Content) != 2 || human[0].Content[1].Type != message.BlockImage || string(human[0].Content[1].Data) != string(image.Data) {
			return fmt.Errorf("model-facing media blocks = %#v", human[0].Content)
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("first response")}})

	firstSaver, err := checkpointsqlite.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	firstAgent, err := New(Options{
		Model: firstModel, Saver: firstSaver, HumanMessageTokenLimit: &limit,
		DisableSubagents: true, DisableSummary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "human-eviction"}
	first, err := firstAgent.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{{
		Role: message.RoleHuman,
		Content: []message.ContentBlock{
			{Type: message.BlockText, Text: firstText}, image, {Type: message.BlockText, Text: secondText},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := firstSaver.Close(); err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 2 || messageText(first.Messages[0]) != fullText {
		t.Fatalf("persisted messages = %#v", first.Messages)
	}
	evictedPath := evictedMessagePath(first.Messages[0])
	if !strings.HasPrefix(evictedPath, "/conversation_history/") || !strings.HasSuffix(evictedPath, ".md") {
		t.Fatalf("evicted path = %q", evictedPath)
	}
	files, ok := first.State["files"].(map[string]any)
	if !ok {
		t.Fatalf("files state = %#v", first.State["files"])
	}
	record, ok := files[evictedPath].(map[string]any)
	if !ok || record["content"] != fullText || record["encoding"] != "utf-8" {
		t.Fatalf("evicted file = %#v", files[evictedPath])
	}

	secondModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		human := onlyRole(request.Messages, message.RoleHuman)
		if len(human) != 2 {
			return fmt.Errorf("replayed human messages = %#v", human)
		}
		if text := human[0].TextContent(); !strings.Contains(text, evictedPath) || strings.Contains(text, "line-10-") {
			return fmt.Errorf("replayed model-facing content = %q", text)
		}
		if human[1].TextContent() != "continue" {
			return fmt.Errorf("new human content = %q", human[1].TextContent())
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("second response")}})
	secondSaver, err := checkpointsqlite.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer secondSaver.Close()
	secondAgent, err := New(Options{
		Model: secondModel, Saver: secondSaver, HumanMessageTokenLimit: &limit,
		DisableSubagents: true, DisableSummary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondAgent.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("continue")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 4 {
		t.Fatalf("replayed messages = %#v", second.Messages)
	}
	ids := map[string]bool{}
	for _, item := range second.Messages {
		if item.ID == "" || ids[item.ID] {
			t.Fatalf("duplicate or empty message ID in %#v", second.Messages)
		}
		ids[item.ID] = true
	}
	if messageText(second.Messages[0]) != fullText || evictedMessagePath(second.Messages[0]) != evictedPath {
		t.Fatalf("replayed persisted human message = %#v", second.Messages[0])
	}
}

func TestFilesystemEvictionLimitsCanBeDisabled(t *testing.T) {
	memory, err := backend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	middleware, err := FilesystemMiddleware(FilesystemOptions{
		Backend: memory, ToolResultTokenLimit: &zero, HumanMessageTokenLimit: &zero,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := agent.ModelRequest{Messages: []message.Message{message.Human(strings.Repeat("x", 1000))}}
	_, err = middleware.WrapModelCall(context.Background(), request, func(_ context.Context, got agent.ModelRequest) (agent.ModelResponse, error) {
		if got.Messages[0].TextContent() != request.Messages[0].TextContent() {
			return agent.ModelResponse{}, fmt.Errorf("disabled human eviction changed request")
		}
		return agent.ModelResponse{Messages: []message.Message{message.Assistant("ok")}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func onlyRole(messages []message.Message, role message.Role) []message.Message {
	result := []message.Message{}
	for _, item := range messages {
		if item.Role == role {
			result = append(result, item)
		}
	}
	return result
}
