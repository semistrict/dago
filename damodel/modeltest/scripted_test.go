package modeltest

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

func TestScriptedInvokeAndStream(t *testing.T) {
	script := New(damodel.Profile{Model: "test"},
		Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
		Step{Chunks: []damodel.Chunk{{MessageDelta: damessage.Assistant("part")}}},
	)
	response, err := script.Invoke(context.Background(), damodel.Request{})
	if err != nil || response.Message.TextContent() != "done" {
		t.Fatalf("Invoke() = %+v, %v", response, err)
	}
	stream, err := script.Stream(context.Background(), damodel.Request{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	chunk, err := stream.Next(context.Background())
	if err != nil || chunk.MessageDelta.TextContent() != "part" {
		t.Fatalf("Next() = %+v, %v", chunk, err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("final Next() error = %v", err)
	}
	if script.Remaining() != 0 {
		t.Fatalf("Remaining() = %d", script.Remaining())
	}
}
