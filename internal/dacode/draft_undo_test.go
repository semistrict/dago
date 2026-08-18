package dacode

import (
	"strings"
	"testing"
)

func TestDraftClearProvidesSingleExactBoundedUndo(t *testing.T) {
	buffer := &draftUndoBuffer{}
	draft := "multiline\n🙂 draft"
	if cleared, ok := buffer.clear(draft); !ok || cleared != "" {
		t.Fatalf("clear = %q, %v", cleared, ok)
	}
	if restored, ok := buffer.undo(); !ok || restored != draft {
		t.Fatalf("undo = %q, %v", restored, ok)
	}
	if restored, ok := buffer.undo(); ok || restored != "" {
		t.Fatalf("second undo = %q, %v", restored, ok)
	}
}

func TestDraftClearRefusesOversizedSnapshotWithoutDataLoss(t *testing.T) {
	buffer := &draftUndoBuffer{}
	draft := strings.Repeat("x", maximumDraftUndoBytes+1)
	if retained, ok := buffer.clear(draft); ok || retained != draft || buffer.ready {
		t.Fatalf("oversized clear = len %d, %v ready=%v", len(retained), ok, buffer.ready)
	}
}
