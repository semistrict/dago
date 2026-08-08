package modeltest

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
)

func TestScriptedInvokeAndStream(t *testing.T) {
	script := New(model.Profile{Model: "test"},
		Step{Response: model.Response{Message: message.Assistant("done")}},
		Step{Chunks: []model.Chunk{{MessageDelta: message.Assistant("part")}}},
	)
	response, err := script.Invoke(context.Background(), model.Request{})
	if err != nil || response.Message.TextContent() != "done" {
		t.Fatalf("Invoke() = %+v, %v", response, err)
	}
	stream, err := script.Stream(context.Background(), model.Request{})
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
