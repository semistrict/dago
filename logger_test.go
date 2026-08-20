package dago

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func TestWithLoggerRoutesEnabledGraphDebugEvents(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	agent := New(
		modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}}),
		WithDebug(),
		WithLogger(logger),
	)
	if _, err := agent.Invoke(t.Context(), dagent.Prompt("hello")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "agent graph event") {
		t.Fatalf("owned logger did not receive debug events: %q", output.String())
	}
}

func TestWithLoggerRejectsNil(t *testing.T) {
	requirePanicContaining(t, "logger is nil", func() { WithLogger(nil) })
}
