package message

import (
	"errors"
	"reflect"
	"testing"
)

func TestReducerMergeAssignsReplacesRemovesAndAppends(t *testing.T) {
	nextID := 0
	reducer := Reducer{IDs: func() (string, error) {
		nextID++
		return "generated-" + string(rune('0'+nextID)), nil
	}}
	left := []Message{
		{ID: "keep", Role: RoleHuman, Content: []ContentBlock{{Type: BlockText, Text: "old"}}},
		{ID: "remove", Role: RoleAssistant},
		{Role: RoleSystem},
	}
	right := []Message{
		{ID: "keep", Role: RoleHuman, Content: []ContentBlock{{Type: BlockText, Text: "new"}}},
		Remove("remove"),
		{Role: RoleAssistant},
	}

	got, err := reducer.Merge(left, right)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Merge() length = %d, want 3", len(got))
	}
	if got[0].TextContent() != "new" || got[0].ID != "keep" {
		t.Fatalf("replaced message = %+v", got[0])
	}
	if got[1].ID != "generated-1" || got[2].ID != "generated-2" {
		t.Fatalf("generated IDs = %q, %q", got[1].ID, got[2].ID)
	}
}

func TestReducerMergeResetUsesChangesAfterLastReset(t *testing.T) {
	reducer := Reducer{}
	right := []Message{
		{ID: "discarded", Role: RoleHuman},
		Remove(RemoveAllMessages),
		{ID: "also-discarded", Role: RoleHuman},
		Remove(RemoveAllMessages),
		{ID: "kept", Role: RoleAssistant},
	}

	got, err := reducer.Merge([]Message{{ID: "old", Role: RoleSystem}}, right)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	want := []Message{{ID: "kept", Role: RoleAssistant}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge() = %+v, want %+v", got, want)
	}
}

func TestReducerMergeRejectsUnknownRemoval(t *testing.T) {
	_, err := (Reducer{}).Merge(nil, []Message{Remove("missing")})
	if !errors.Is(err, ErrRemoveUnknownMessage) {
		t.Fatalf("Merge() error = %v, want %v", err, ErrRemoveUnknownMessage)
	}
}

func TestDeltaReduceIsBatchingInvariant(t *testing.T) {
	state := []Message{{ID: "a", Role: RoleHuman, Content: []ContentBlock{{Type: BlockText, Text: "a"}}}}
	xs := [][]Message{{
		{ID: "b", Role: RoleAssistant, Content: []ContentBlock{{Type: BlockText, Text: "b"}}},
		{ID: "a", Role: RoleHuman, Content: []ContentBlock{{Type: BlockText, Text: "updated"}}},
	}}
	ys := [][]Message{{Remove("b"), {Role: RoleAssistant, Content: []ContentBlock{{Type: BlockText, Text: "anonymous"}}}}}

	first, err := DeltaReduce(state, xs)
	if err != nil {
		t.Fatalf("DeltaReduce(xs) error = %v", err)
	}
	separate, err := DeltaReduce(first, ys)
	if err != nil {
		t.Fatalf("DeltaReduce(ys) error = %v", err)
	}
	combined, err := DeltaReduce(state, append(xs, ys...))
	if err != nil {
		t.Fatalf("DeltaReduce(combined) error = %v", err)
	}
	if !reflect.DeepEqual(separate, combined) {
		t.Fatalf("separate = %+v, combined = %+v", separate, combined)
	}
}

func TestMessageCloneIsDeep(t *testing.T) {
	original := Message{
		Role:      RoleAssistant,
		Content:   []ContentBlock{{Type: BlockImage, Data: []byte{1, 2}}},
		ToolCalls: []ToolCall{{ID: "call", Name: "tool", Arguments: []byte(`{"x":1}`)}},
	}
	clone := original.Clone()
	clone.Content[0].Data[0] = 9
	clone.ToolCalls[0].Arguments[0] = '['
	if original.Content[0].Data[0] != 1 || original.ToolCalls[0].Arguments[0] != '{' {
		t.Fatal("Clone() shares nested storage")
	}
}
