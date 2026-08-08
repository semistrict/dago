package model

import (
	"context"
	"errors"
	"io"
	"testing"
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
