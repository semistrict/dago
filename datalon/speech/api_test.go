package speech

import (
	"net/http"
	"testing"
)

func TestConstructorsRejectNegativeLimits(t *testing.T) {
	for name, call := range map[string]func(){
		"remote":  func() { NewOpenAI(http.DefaultClient, "key", "model", OpenAIOptions{Timeout: -1}) },
		"local":   func() { NewLocal(LocalOptions{MaxInputBytes: -1}) },
		"channel": func() { NewChannel(&stubChannel{}, &stubTranscriber{}, ChannelOptions{MaxTranscriptBytes: -1}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("negative static limit did not panic")
				}
			}()
			call()
		})
	}
}
