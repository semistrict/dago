package dagent

import (
	"context"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func TestEventsClosesOnEarlyBreak(t *testing.T) {
	model := modeltest.New(damodel.Profile{}, modeltest.Step{
		Response: damodel.Response{Message: damessage.Assistant("done")},
	})
	agent := New(model, Options{})
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
