package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/semistrict/dago/checkpoint"
	checkpointpostgres "github.com/semistrict/dago/checkpoint/postgres"
	"github.com/semistrict/dago/checkpoint/sqlite"
	"github.com/semistrict/dago/message"
)

func main() {
	if len(os.Args) != 2 && len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: checkpoint-fixture OUTPUT.sqlite | checkpoint-fixture postgres DSN")
		os.Exit(2)
	}
	var saver interface {
		checkpoint.Saver
		Close() error
	}
	threadID := "go-safe"
	var err error
	if len(os.Args) == 3 && os.Args[1] == "postgres" {
		saver, err = checkpointpostgres.Open(os.Args[2])
		threadID = "go-safe-postgres"
	} else if len(os.Args) == 2 {
		saver, err = sqlite.Open(os.Args[1])
	} else {
		fmt.Fprintln(os.Stderr, "unknown checkpoint fixture target")
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
	defer saver.Close()

	values := map[string]any{
		"scalar": "go",
		"bytes":  []byte{0, 1, 's', 'a', 'f', 'e'},
		"nested": map[string]any{"items": []any{1, true, nil, 3.25}},
		"messages": []any{message.Message{
			ID: "human-1", Role: message.RoleHuman,
			Content:  []message.ContentBlock{{Type: message.BlockText, Text: "hello from Go"}},
			Metadata: map[string]json.RawMessage{"fixture": json.RawMessage(`true`)},
		}},
		"delta": checkpoint.DeltaSnapshot{Value: []any{"seed", map[string]any{"count": 1}}},
	}
	versions := make(map[string]string, len(values))
	seen := make(map[string]string, len(values))
	updated := make([]string, 0, len(values))
	for key := range values {
		versions[key] = "00000000000000000000000000000001.0.1"
		seen[key] = versions[key]
		updated = append(updated, key)
	}
	value := checkpoint.Checkpoint{
		Version: 4, ID: "1f000001-0000-6000-8000-000000000001",
		Timestamp: "2026-08-08T12:01:00+00:00", ChannelValues: values,
		ChannelVersions: versions, VersionsSeen: map[string]map[string]string{"writer": seen},
		UpdatedChannels: updated,
	}
	ctx := context.Background()
	if err := saver.DeleteThread(ctx, threadID); err != nil {
		fail(err)
	}
	config, err := saver.Put(ctx, checkpoint.Config{ThreadID: threadID}, value, checkpoint.Metadata{
		Source: "input", Step: 1, Extra: map[string]any{"fixture_owner": "go"},
	}, value.ChannelVersions)
	if err != nil {
		fail(err)
	}
	if err := saver.PutWrites(ctx, config, "go-task", "ignored-by-sqlite", []checkpoint.ChannelWrite{
		{Channel: "delta", Value: []any{"write-one"}},
		{Channel: "plain", Value: map[string]any{"source": "go", "ok": true}},
		{Channel: "binary", Value: []byte("pending-bytes")},
	}); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
