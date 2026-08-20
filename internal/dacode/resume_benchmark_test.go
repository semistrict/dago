package dacode

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

const (
	resumeBenchmarkThreadEnvironment = "DAGO_BENCHMARK_RESUME_THREAD"
	resumeBenchmarkStateEnvironment  = "DAGO_BENCHMARK_STATE_DIR"
)

var resumeBenchmarkViewSink string

// BenchmarkResumeThread measures checkpoint reconstruction plus the application
// resume and first-frame render paths. The deterministic fixture mirrors a real
// 96-message thread with about 836 KiB of text and 49 tool calls. Set
// DAGO_BENCHMARK_RESUME_THREAD to also benchmark a read-only load of an existing
// thread; DAGO_BENCHMARK_STATE_DIR optionally overrides the usual state directory.
func BenchmarkResumeThread(b *testing.B) {
	b.Run("realistic_fixture", func(b *testing.B) {
		messages := realisticResumeBenchmarkMessages()
		b.ReportAllocs()
		for b.Loop() {
			resumeBenchmarkViewSink = resumeBenchmarkFrame(b, "benchmark-thread", messages)
		}
		reportResumeBenchmarkScale(b, messages)
	})

	threadID := strings.TrimSpace(os.Getenv(resumeBenchmarkThreadEnvironment))
	if threadID == "" {
		return
	}
	b.Run("existing_thread", func(b *testing.B) {
		load := openExistingResumeBenchmark(b, threadID)
		messages := load()
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			resumeBenchmarkViewSink = resumeBenchmarkFrame(b, threadID, load())
		}
		reportResumeBenchmarkScale(b, messages)
	})
}

func resumeBenchmarkFrame(b *testing.B, threadID string, messages []damessage.Message) string {
	b.Helper()
	model := newTUIModel(b.Context(), &fakeRunner{}, "/work", "openai:model", "new-thread", false, false, "")
	model.resize(120, 40)
	model.sessionPicker = &sessionPickerState{resuming: true, startup: true}
	model.Update(sessionLoadedMsg{
		session:  sessionInfo{ThreadID: threadID, CheckpointID: "benchmark-checkpoint", MessageCount: len(messages)},
		messages: messages,
	})
	return model.View()
}

func openExistingResumeBenchmark(b *testing.B, threadID string) func() []damessage.Message {
	b.Helper()
	if err := validateResumeThreadID(threadID); err != nil {
		b.Fatalf("%s is invalid: %v", resumeBenchmarkThreadEnvironment, err)
	}
	stateDirectory := strings.TrimSpace(os.Getenv(resumeBenchmarkStateEnvironment))
	if stateDirectory == "" {
		configurationDirectory, err := os.UserConfigDir()
		if err != nil {
			b.Fatal(err)
		}
		stateDirectory = filepath.Join(configurationDirectory, "dacode")
	}
	databasePath := filepath.Join(stateDirectory, "threads.db")
	if info, err := os.Stat(databasePath); err != nil || !info.Mode().IsRegular() {
		b.Fatalf("open benchmark database %q: %v", databasePath, err)
	}
	databaseURL := (&url.URL{Scheme: "file", Path: databasePath, RawQuery: "mode=ro"}).String()
	database, err := sql.Open("sqlite", databaseURL)
	if err != nil {
		b.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	b.Cleanup(func() {
		if err := database.Close(); err != nil {
			b.Errorf("close benchmark database: %v", err)
		}
	})
	agent := dagent.New(modeltest.New(damodel.Profile{}, modeltest.Step{}), dagent.Options{
		Saver: checkpointsqlite.New(database), StateFields: sessionStateFields(),
	})
	return func() []damessage.Message {
		snapshot, err := agent.State(b.Context(), dacheckpoint.Config{ThreadID: threadID})
		if err != nil {
			b.Fatalf("reconstruct benchmark thread: %v", err)
		}
		messages, err := decodeSessionMessages(snapshot.State[dagent.MessagesKey])
		if err != nil {
			b.Fatalf("decode benchmark thread: %v", err)
		}
		return messages
	}
}

func reportResumeBenchmarkScale(b *testing.B, messages []damessage.Message) {
	b.Helper()
	textBytes := 0
	for _, message := range messages {
		textBytes += len(message.TextContent())
	}
	b.SetBytes(int64(textBytes))
	b.ReportMetric(float64(len(messages)), "messages/thread")
	b.ReportMetric(float64(textBytes)/1024, "KiB/thread")
}

func realisticResumeBenchmarkMessages() []damessage.Message {
	const (
		humanMessages     = 12
		assistantMessages = 35
		toolMessages      = 49
	)
	messages := make([]damessage.Message, 0, humanMessages+assistantMessages+toolMessages)
	assistantIndex, toolIndex := 0, 0
	for turn := range humanMessages {
		humanBytes := 2_763
		if turn == humanMessages-1 {
			humanBytes = 2_767
		}
		messages = append(messages, damessage.Human(resumeBenchmarkText(fmt.Sprintf("request %02d", turn), humanBytes)))

		turnAssistants := 3
		if turn == humanMessages-1 {
			turnAssistants = 2
		}
		for range turnAssistants {
			assistantBytes := 309
			if assistantIndex == assistantMessages-1 {
				assistantBytes = 308
			}
			assistant := damessage.Assistant(resumeBenchmarkText(fmt.Sprintf("assistant %02d", assistantIndex), assistantBytes))
			if assistantIndex < 6 {
				assistant.Content = append(assistant.Content, damessage.ContentBlock{Type: damessage.BlockReasoning, Reasoning: "checked the workspace and selected the next tool"})
			}
			callCount := 1
			if assistantIndex < 14 {
				callCount = 2
			}
			callIDs := make([]string, callCount)
			for call := range callCount {
				callIDs[call] = fmt.Sprintf("benchmark-call-%02d", toolIndex+call)
				assistant.ToolCalls = append(assistant.ToolCalls, damessage.ToolCall{
					ID: callIDs[call], Name: "execute", Arguments: json.RawMessage(`{"command":"inspect the workspace"}`),
				})
			}
			messages = append(messages, assistant)
			for _, callID := range callIDs {
				toolBytes := 11_823
				switch {
				case toolIndex == 0:
					toolBytes = 244_795
				case toolIndex > 44:
					toolBytes = 11_822
				}
				tool := damessage.Tool(callID, resumeBenchmarkText(fmt.Sprintf("tool output %02d", toolIndex), toolBytes))
				tool.Name = "execute"
				messages = append(messages, tool)
				toolIndex++
			}
			assistantIndex++
		}
	}
	if len(messages) != humanMessages+assistantMessages+toolMessages || assistantIndex != assistantMessages || toolIndex != toolMessages {
		panic("resume benchmark fixture shape is invalid")
	}
	return messages
}

func resumeBenchmarkText(label string, size int) string {
	line := label + " " + strings.Repeat("x", 72) + "\n"
	if len(line) >= size {
		return line[:size]
	}
	value := strings.Repeat(line, size/len(line)+1)
	return value[:size]
}
