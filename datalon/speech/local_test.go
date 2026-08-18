package speech

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/dago/datalon"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls []runnerCall
	block bool
}

type runnerCall struct {
	name string
	args []string
}

func (runner *fakeRunner) Run(ctx context.Context, name string, arguments []string, _ int) ([]byte, []byte, error) {
	runner.mu.Lock()
	runner.calls = append(runner.calls, runnerCall{name: name, args: append([]string(nil), arguments...)})
	runner.mu.Unlock()
	if runner.block {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	if name == "ffmpeg-test" {
		if err := os.WriteFile(arguments[len(arguments)-1], []byte("wav"), 0o600); err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	}
	return []byte(" hello world \n"), nil, nil
}

func TestLocalTranscriberUsesFixedConversionAndPinnedDefaults(t *testing.T) {
	t.Parallel()
	media := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(media, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	transcriber := NewLocal(LocalOptions{FFmpeg: "ffmpeg-test", Python: "python-test", Runner: runner})
	text, err := transcriber.Transcribe(t.Context(), datalon.Message{Metadata: map[string]any{"voice_path": media}})
	if err != nil || text != "hello world" {
		t.Fatalf("Transcribe = %q, %v", text, err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.calls) != 2 || runner.calls[0].name != "ffmpeg-test" || runner.calls[1].name != "python-test" {
		t.Fatalf("calls = %+v", runner.calls)
	}
	ffmpeg := strings.Join(runner.calls[0].args, " ")
	if !strings.Contains(ffmpeg, "-ar 16000 -ac 1 -f wav") {
		t.Fatalf("ffmpeg args = %s", ffmpeg)
	}
	python := runner.calls[1].args
	if python[2] != DefaultLocalModel || python[3] != "cpu" || !strings.Contains(python[1], "transformers") {
		t.Fatalf("python args = %q", python)
	}
	if _, err := os.Stat(python[4]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary WAV remains: %v", err)
	}
	if _, err := os.Stat(runner.calls[0].args[6]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary input remains: %v", err)
	}
}

func TestLocalTranscriberRejectsSymlinkAndHonorsCancellation(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "link")
	if err := os.Symlink(target, symlink); err == nil {
		transcriber := NewLocal(LocalOptions{Runner: &fakeRunner{}})
		if _, err := transcriber.Transcribe(t.Context(), datalon.Message{Metadata: map[string]any{"voice_path": symlink}}); !errors.Is(err, ErrInvalidMedia) {
			t.Fatalf("symlink error = %v", err)
		}
	}
	runner := &fakeRunner{block: true}
	transcriber := NewLocal(LocalOptions{FFmpeg: "ffmpeg-test", Runner: runner})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transcriber.Transcribe(ctx, datalon.Message{Metadata: map[string]any{"voice_path": target}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestLimitedBufferEnforcesBound(t *testing.T) {
	t.Parallel()
	buffer := &limitedBuffer{limit: 4}
	if _, err := buffer.Write([]byte("12345")); !errors.Is(err, ErrTranscriptionBound) || string(buffer.Bytes()) != "1234" {
		t.Fatalf("Write = %q, %v", buffer.Bytes(), err)
	}
}
