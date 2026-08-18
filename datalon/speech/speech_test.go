package speech

import (
	"context"
	"errors"
	"testing"

	"github.com/semistrict/dago/datalon"
)

type stubTranscriber struct {
	text  string
	err   error
	calls int
}

func (transcriber *stubTranscriber) Transcribe(context.Context, datalon.Message) (string, error) {
	transcriber.calls++
	return transcriber.text, transcriber.err
}

func TestConfigFromEnvSelectsLocalAndRemoteModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		env  map[string]string
		want Config
	}{
		{env: map[string]string{}, want: Config{}},
		{env: map[string]string{"SPEECH_ENABLED": "yes", "SPEECH_DEVICE": "cuda"}, want: Config{Enabled: true, Local: true, Model: DefaultLocalModel, Device: "cuda"}},
		{env: map[string]string{"DEEPAGENTS_TALON_VOICE_TRANSCRIPTION_ENABLED": "true", "DEEPAGENTS_TALON_VOICE_TRANSCRIPTION_MODEL": "gpt-4o-transcribe"}, want: Config{Enabled: true, Model: "gpt-4o-transcribe", Device: "cpu"}},
	}
	for _, testCase := range tests {
		got, err := ConfigFromEnv(testCase.env)
		if err != nil || got != testCase.want {
			t.Fatalf("ConfigFromEnv(%v) = %+v, %v; want %+v", testCase.env, got, err, testCase.want)
		}
	}
	if _, err := ConfigFromEnv(map[string]string{"SPEECH_ENABLED": "sometimes"}); err == nil {
		t.Fatal("invalid boolean accepted")
	}
}

func TestTranscribeMessageEligibilityAppendAndIsolation(t *testing.T) {
	t.Parallel()
	transcriber := &stubTranscriber{text: " transcript "}
	message := datalon.Message{
		ConversationID: "chat", Text: "caption",
		Metadata: map[string]any{"media_type": "video", "media_path": "clip.mp4"},
	}
	updated, err := TranscribeMessage(t.Context(), transcriber, message, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Text != "caption\n\ntranscript" || updated.Metadata["voice_transcribed"] != true || transcriber.calls != 1 {
		t.Fatalf("updated = %+v, calls = %d", updated, transcriber.calls)
	}
	if _, exists := message.Metadata["voice_transcribed"]; exists {
		t.Fatal("input metadata was mutated")
	}
	plain := datalon.Message{Text: "doc", Metadata: map[string]any{"media_type": "document", "media_path": "report.pdf"}}
	if got, err := TranscribeMessage(t.Context(), transcriber, plain, 1024); err != nil || got.Text != plain.Text || transcriber.calls != 1 {
		t.Fatalf("plain message = %+v, %v, calls = %d", got, err, transcriber.calls)
	}
	transcriber.text = string(make([]byte, 5))
	if _, err := TranscribeMessage(t.Context(), transcriber, message, 4); !errors.Is(err, ErrTranscriptionBound) {
		t.Fatalf("bound error = %v", err)
	}
}

type stubChannel struct {
	handler datalon.Handler
	stopped bool
	sends   int
}

func (*stubChannel) ID() string { return "stub" }
func (channel *stubChannel) Start(_ context.Context, handler datalon.Handler) error {
	channel.handler = handler
	return nil
}
func (channel *stubChannel) Stop(context.Context) error { channel.stopped = true; return nil }
func (channel *stubChannel) Send(context.Context, string, string) datalon.SendResult {
	channel.sends++
	return datalon.SendResult{Success: true}
}

func TestChannelBestEffortAndStrictFailures(t *testing.T) {
	t.Parallel()
	message := datalon.Message{Text: "voice", Metadata: map[string]any{"voice_path": "missing"}}
	want := errors.New("offline")
	for _, strict := range []bool{false, true} {
		inner := &stubChannel{}
		transcriber := &stubTranscriber{err: want}
		reported := 0
		wrapped := NewChannel(inner, transcriber, ChannelOptions{Strict: strict, OnError: func(err error) {
			if errors.Is(err, want) {
				reported++
			}
		}})
		handled := 0
		if err := wrapped.Start(t.Context(), func(_ context.Context, got datalon.Message) error { handled++; return nil }); err != nil {
			t.Fatal(err)
		}
		err := inner.handler(t.Context(), message)
		if strict && !errors.Is(err, want) {
			t.Fatalf("strict error = %v", err)
		}
		if !strict && (err != nil || handled != 1) {
			t.Fatalf("best effort = %v, handled %d", err, handled)
		}
		if reported != 1 {
			t.Fatalf("reported = %d", reported)
		}
		if wrapped.Send(t.Context(), "chat", "text").Success != true || wrapped.Stop(t.Context()) != nil || !inner.stopped {
			t.Fatal("wrapper did not delegate lifecycle")
		}
	}
}

func TestChannelRejectsTypedNilDependencies(t *testing.T) {
	t.Parallel()
	var channel *stubChannel
	var transcriber *stubTranscriber
	for name, call := range map[string]func(){
		"channel":     func() { NewChannel(channel, &stubTranscriber{}, ChannelOptions{}) },
		"transcriber": func() { NewChannel(&stubChannel{}, transcriber, ChannelOptions{}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewChannel did not panic")
				}
			}()
			call()
		})
	}
}
