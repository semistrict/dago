package dago

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
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
	image := damessage.ContentBlock{Type: damessage.BlockImage, MIMEType: "image/png", Data: []byte{1, 2, 3}}

	firstModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		human := onlyRole(request.Messages, damessage.RoleHuman)
		if len(human) != 1 {
			return fmt.Errorf("human messages = %#v", human)
		}
		if text := human[0].TextContent(); !strings.Contains(text, "Message content too large") || !strings.Contains(text, "10 lines truncated") || strings.Contains(text, "line-10-") {
			return fmt.Errorf("model-facing human content = %q", text)
		}
		if len(human[0].Content) != 2 || human[0].Content[1].Type != damessage.BlockImage || string(human[0].Content[1].Data) != string(image.Data) {
			return fmt.Errorf("model-facing media blocks = %#v", human[0].Content)
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("first response")}})

	firstSaver, err := checkpointsqlite.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	firstAgent := NewAgent(
		firstModel, WithSaver(firstSaver), WithFilesystem(Filesystem{HumanMessageTokenLimit: limit}), WithoutSubagents(), WithoutSummary(),
	)
	config := dacheckpoint.Config{ThreadID: "human-eviction"}
	first, err := firstAgent.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{{
		Role: damessage.RoleHuman,
		Content: []damessage.ContentBlock{
			{Type: damessage.BlockText, Text: firstText}, image, {Type: damessage.BlockText, Text: secondText},
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

	secondModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		human := onlyRole(request.Messages, damessage.RoleHuman)
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
	}, Response: damodel.Response{Message: damessage.Assistant("second response")}})
	secondSaver, err := checkpointsqlite.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer secondSaver.Close()
	secondAgent := NewAgent(
		secondModel, WithSaver(secondSaver), WithFilesystem(Filesystem{HumanMessageTokenLimit: limit}), WithoutSubagents(), WithoutSummary(),
	)
	second, err := secondAgent.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("continue")}})
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
	memory := dabackend.NewMemory(nil)

	middleware := mustFilesystem(
		memory, Filesystem{
			ToolResultLimit: ContentLimit{Unit: ContentTokens, Amount: -1}, HumanMessageTokenLimit: -1,
		})
	request := dagent.ModelRequest{Messages: []damessage.Message{damessage.Human(strings.Repeat("x", 1000))}}
	_, err := middleware.WrapModelCall(context.Background(), request, func(_ context.Context, got dagent.ModelRequest) (dagent.ModelResponse, error) {
		if got.Messages[0].TextContent() != request.Messages[0].TextContent() {
			return dagent.ModelResponse{}, fmt.Errorf("disabled human eviction changed request")
		}
		return dagent.ModelResponse{Messages: []damessage.Message{damessage.Assistant("ok")}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func onlyRole(messages []damessage.Message, role damessage.Role) []damessage.Message {
	result := []damessage.Message{}
	for _, item := range messages {
		if item.Role == role {
			result = append(result, item)
		}
	}
	return result
}
