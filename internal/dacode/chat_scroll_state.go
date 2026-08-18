package dacode

const (
	maxChatScrollLines = 10_000_000
	defaultWheelLines  = 3
)

type chatScrollState struct {
	Offset         int
	MaxOffset      int
	FollowBottom   bool
	ContentLines   int
	ViewportHeight int
}

func newChatScrollState() chatScrollState {
	return chatScrollState{FollowBottom: true}
}

// updateLayout reconciles scroll position after transcript content or viewport
// height changes. Bottom-follow stays armed while short content is top-aligned,
// then engages on the first overflow. A user-released offset remains stable.
func (state *chatScrollState) updateLayout(contentLines, viewportHeight int) int {
	return state.updateLayoutPreservingAnchor(contentLines, viewportHeight, 0)
}

// updateLayoutPreservingAnchor keeps the same transcript content visible when
// older lines are inserted above a manually positioned viewport. Streaming
// appends pass zero and therefore never drag a released viewport.
func (state *chatScrollState) updateLayoutPreservingAnchor(contentLines, viewportHeight, insertedAbove int) int {
	if state == nil {
		panic("dacode: initialized chat scroll state is required")
	}
	contentLines = min(max(contentLines, 0), maxChatScrollLines)
	viewportHeight = min(max(viewportHeight, 1), maxChatScrollLines)
	insertedAbove = min(max(insertedAbove, 0), maxChatScrollLines)
	state.ContentLines = contentLines
	state.ViewportHeight = viewportHeight
	state.MaxOffset = max(contentLines-viewportHeight, 0)
	if state.FollowBottom {
		state.Offset = state.MaxOffset
	} else {
		state.Offset = min(state.Offset+insertedAbove, maxChatScrollLines)
		state.Offset = min(max(state.Offset, 0), state.MaxOffset)
	}
	return state.Offset
}

func (state *chatScrollState) scrollLines(delta int) int {
	if state == nil {
		panic("dacode: initialized chat scroll state is required")
	}
	delta = min(max(delta, -maxChatScrollLines), maxChatScrollLines)
	state.userScrolled(state.Offset + delta)
	return state.Offset
}

func (state *chatScrollState) wheel(direction int) int {
	direction = min(max(direction, -1), 1)
	return state.scrollLines(direction * defaultWheelLines)
}

func (state *chatScrollState) pageUp(hydrationThreshold int) (int, bool) {
	page := max(state.ViewportHeight-1, 1)
	state.scrollLines(-page)
	return state.Offset, state.shouldHydrateOlder(hydrationThreshold)
}

// userScrolled records an actual user-controlled offset. Reaching the bottom
// re-arms follow mode; any offset above it releases follow mode.
func (state *chatScrollState) userScrolled(offset int) {
	if state == nil {
		panic("dacode: initialized chat scroll state is required")
	}
	state.Offset = min(max(offset, 0), state.MaxOffset)
	state.FollowBottom = state.Offset >= state.MaxOffset
}

func (state chatScrollState) shouldHydrateOlder(threshold int) bool {
	threshold = min(max(threshold, 0), maxChatScrollLines)
	return state.MaxOffset > 0 && state.Offset <= threshold
}

func (state chatScrollState) thumb(trackHeight int) (start, size int) {
	trackHeight = min(max(trackHeight, 0), maxChatScrollLines)
	if trackHeight == 0 || state.ContentLines <= 0 || state.ViewportHeight >= state.ContentLines {
		return 0, trackHeight
	}
	size = max(state.ViewportHeight*trackHeight/state.ContentLines, 1)
	size = min(size, trackHeight)
	travel := trackHeight - size
	if state.MaxOffset > 0 {
		start = min(max(state.Offset, 0), state.MaxOffset) * travel / state.MaxOffset
	}
	return min(max(start, 0), travel), size
}
