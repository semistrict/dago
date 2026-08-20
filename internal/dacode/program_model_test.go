package dacode

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

type renderProbeWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	writes chan struct{}
}

func newRenderProbeWriter() *renderProbeWriter {
	return &renderProbeWriter{writes: make(chan struct{}, 32)}
}

func (writer *renderProbeWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	_, _ = writer.buffer.Write(value)
	writer.mu.Unlock()
	select {
	case writer.writes <- struct{}{}:
	default:
	}
	return len(value), nil
}

func (writer *renderProbeWriter) reset() {
	writer.mu.Lock()
	writer.buffer.Reset()
	writer.mu.Unlock()
	for {
		select {
		case <-writer.writes:
		default:
			return
		}
	}
}

func (writer *renderProbeWriter) snapshot() []byte {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]byte(nil), writer.buffer.Bytes()...)
}

type animationProbeModel struct {
	programModel
}

func (animationProbeModel) Init() tea.Cmd { return nil }

func (model animationProbeModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	_, _ = model.model.Update(message)
	return model, nil
}

func TestProgramModelSpinnerTickUsesLocalizedRendererUpdate(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.running = true
	model.status = "Thinking"
	model.refreshTranscript()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	output := newRenderProbeWriter()
	program := tea.NewProgram(
		animationProbeModel{programModel: newProgramModel(model)},
		tea.WithContext(ctx),
		tea.WithInput(nil),
		tea.WithOutput(output),
		tea.WithWindowSize(80, 24),
	)
	done := make(chan error, 1)
	go func() {
		_, err := program.Run()
		done <- err
	}()

	waitForRenderProbe(t, output, func(value []byte) bool {
		return bytes.Contains(value, []byte("Thinking"))
	})
	output.reset()
	program.Send(spinner.TickMsg{})
	waitForRenderProbe(t, output, func(value []byte) bool { return len(value) > 0 })
	tickOutput := output.snapshot()

	cancel()
	select {
	case err := <-done:
		if err != nil && ctx.Err() == nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("program did not stop")
	}

	if lineFeeds := bytes.Count(tickOutput, []byte("\n")); lineFeeds >= model.height-1 {
		t.Fatalf("spinner tick traversed all %d terminal rows (%d line feeds, %d output bytes)", model.height, lineFeeds, len(tickOutput))
	}
}

func waitForRenderProbe(t *testing.T, writer *renderProbeWriter, ready func([]byte) bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !ready(writer.snapshot()) {
		select {
		case <-writer.writes:
		case <-deadline:
			t.Fatal("renderer output timed out")
		}
	}
}
