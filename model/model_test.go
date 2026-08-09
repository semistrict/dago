package model

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/semistrict/dago/message"
)

func TestEmptyStream(t *testing.T) {
	stream := EmptyStream{}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() error = %v, want io.EOF", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestOutcomeRoundTrip(t *testing.T) {
	item := message.Assistant("")
	SetOutcome(&item, FinishReasonRefusal, &Refusal{Category: "policy", Explanation: "not allowed"})
	reason, refusal := Outcome(item)
	if reason != FinishReasonRefusal || refusal == nil || refusal.Category != "policy" || refusal.Explanation != "not allowed" {
		t.Fatalf("outcome = %q, %#v", reason, refusal)
	}
}
