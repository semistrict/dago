package docker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDockerSandboxLive uses an explicitly selected, already-local image. It
// never pulls an image and therefore remains opt-in in ordinary test runs.
func TestDockerSandboxLive(t *testing.T) {
	image := os.Getenv("DAGO_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("DAGO_DOCKER_TEST_IMAGE is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backend, err := New(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("close Docker sandbox: %v", err)
		}
	})
	if _, err := backend.Write(ctx, "/input.txt", "hello Docker\n"); err != nil {
		t.Fatal(err)
	}
	result, err := backend.Execute(ctx, "tr '[:lower:]' '[:upper:]' < input.txt > output.txt", 5*time.Second)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("execute = %#v, %v", result, err)
	}
	read, err := backend.Read(ctx, "/output.txt", 0, 10)
	if err != nil || read.Data == nil || strings.TrimSpace(read.Data.Content) != "HELLO DOCKER" {
		t.Fatalf("read = %#v, %v", read, err)
	}
}
