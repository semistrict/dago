package damodel

import (
	"context"
	"errors"
	"io"
	"iter"
	"testing"
)

type iteratorStream struct {
	chunks []Chunk
	next   int
	err    error
	closed bool
}

func (stream *iteratorStream) Next(context.Context) (Chunk, error) {
	if stream.next < len(stream.chunks) {
		chunk := stream.chunks[stream.next]
		stream.next++
		return chunk, nil
	}
	if stream.err != nil {
		err := stream.err
		stream.err = nil
		return Chunk{}, err
	}
	return Chunk{}, io.EOF
}

func (stream *iteratorStream) Close() error {
	stream.closed = true
	return nil
}

func (stream *iteratorStream) Chunks() iter.Seq2[Chunk, error] {
	return Chunks(context.Background(), stream)
}

func TestChunksClosesOnEarlyBreak(t *testing.T) {
	stream := &iteratorStream{chunks: []Chunk{{Done: false}, {Done: true}}}
	count := 0
	for _, err := range stream.Chunks() {
		if err != nil {
			t.Fatal(err)
		}
		count++
		break
	}
	if count != 1 || !stream.closed || stream.next != 1 {
		t.Fatalf("iteration count=%d closed=%v next=%d", count, stream.closed, stream.next)
	}
}

func TestChunksYieldsTerminalErrorAndCloses(t *testing.T) {
	want := errors.New("stream failed")
	stream := &iteratorStream{err: want}
	var got error
	for _, err := range stream.Chunks() {
		got = err
	}
	if !errors.Is(got, want) || !stream.closed {
		t.Fatalf("error=%v closed=%v", got, stream.closed)
	}
}
