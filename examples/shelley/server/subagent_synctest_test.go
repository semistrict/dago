package server

import (
	"testing"
	"time"

	"github.com/semistrict/dago/damodel/modeltest"
)

func TestSubagentIdlePollingWithFakeTime(t *testing.T) {
	modeltest.TestWithFakeTime(t, func(t *testing.T) {
		manager := &ConversationManager{agentWorking: true}
		runner := &SubagentRunner{}
		started := time.Now()
		done, err := runner.waitForIdle(t.Context(), manager, "subagent", started.Add(2*time.Second))
		if err != nil || done || time.Since(started) != 2*time.Second {
			t.Fatalf("done=%v elapsed=%s error=%v", done, time.Since(started), err)
		}
	})
}
