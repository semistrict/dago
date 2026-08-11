package dacache

import (
	"context"
	"testing"
	"time"
)

func TestMemoryCopiesAndExpires(t *testing.T) {
	memory := NewMemory()
	now := time.Unix(100, 0)
	memory.now = func() time.Time { return now }
	input := []byte("value")
	if err := memory.Set(context.Background(), "key", input, time.Second); err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	value, ok, err := memory.Get(context.Background(), "key")
	if err != nil || !ok || string(value) != "value" {
		t.Fatalf("get = %q, %v, %v", value, ok, err)
	}
	value[0] = 'Y'
	value, ok, _ = memory.Get(context.Background(), "key")
	if !ok || string(value) != "value" {
		t.Fatalf("cache returned shared bytes: %q", value)
	}
	now = now.Add(time.Second)
	_, ok, err = memory.Get(context.Background(), "key")
	if err != nil || ok {
		t.Fatalf("expired get = %v, %v", ok, err)
	}
}
