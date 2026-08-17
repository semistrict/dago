package dagent

import (
	"context"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

type blockingStreamTestModel struct{}

func (blockingStreamTestModel) Invoke(ctx context.Context, _ damodel.Request) (damodel.Response, error) {
	<-ctx.Done()
	return damodel.Response{}, ctx.Err()
}

func (blockingStreamTestModel) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	panic("unexpected streaming model call")
}

func (blockingStreamTestModel) Profile() damodel.Profile { return damodel.Profile{} }

func TestEventsClosesOnEarlyBreak(t *testing.T) {
	agent := New(blockingStreamTestModel{}, Options{})
	stream := agent.Stream(t.Context(), Input{Messages: []damessage.Message{damessage.Human("go")}})
	count := 0
	for _, err := range stream.Events() {
		if err != nil {
			t.Fatal(err)
		}
		count++
		break
	}
	if count != 1 {
		t.Fatalf("event count = %d", count)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := stream.Result(ctx); err == nil {
		t.Fatal("Result() succeeded after iterator closed execution early")
	}
}
