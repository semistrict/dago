package dacode

import "strings"

const maximumDraftUndoBytes = 1 << 20

// draftUndoBuffer owns a single exact draft snapshot. Oversized drafts are
// not cleared, preventing a bounded undo policy from silently restoring only
// part of a user's input.
type draftUndoBuffer struct {
	value string
	ready bool
}

func (buffer *draftUndoBuffer) clear(draft string) (cleared string, ok bool) {
	if buffer == nil {
		panic("dacode: initialized draft undo buffer is required")
	}
	if draft == "" || len(draft) > maximumDraftUndoBytes {
		return draft, false
	}
	buffer.value = strings.Clone(draft)
	buffer.ready = true
	return "", true
}

func (buffer *draftUndoBuffer) undo() (string, bool) {
	if buffer == nil {
		panic("dacode: initialized draft undo buffer is required")
	}
	if !buffer.ready {
		return "", false
	}
	value := buffer.value
	buffer.value = ""
	buffer.ready = false
	return value, true
}

func (buffer *draftUndoBuffer) discard() {
	if buffer == nil {
		panic("dacode: initialized draft undo buffer is required")
	}
	buffer.value = ""
	buffer.ready = false
}
