package dacode

import "testing"

func TestChatScrollTopAlignsShortContentThenFollowsOverflow(t *testing.T) {
	state := newChatScrollState()
	if offset := state.updateLayout(5, 10); offset != 0 || !state.FollowBottom {
		t.Fatalf("short layout = %#v", state)
	}
	if offset := state.updateLayout(15, 10); offset != 5 || !state.FollowBottom {
		t.Fatalf("overflow layout = %#v", state)
	}
	if offset := state.updateLayout(18, 10); offset != 8 {
		t.Fatalf("follow update = %#v", state)
	}
}

func TestChatScrollManualOffsetIsStableAndBottomRearmsFollow(t *testing.T) {
	state := newChatScrollState()
	state.updateLayout(100, 20)
	state.userScrolled(30)
	if state.FollowBottom || state.Offset != 30 {
		t.Fatalf("manual scroll = %#v", state)
	}
	state.updateLayout(120, 20)
	if state.Offset != 30 || state.FollowBottom {
		t.Fatalf("content update moved released scroll = %#v", state)
	}
	state.userScrolled(100)
	if !state.FollowBottom {
		t.Fatalf("bottom did not rearm = %#v", state)
	}
	state.updateLayout(130, 20)
	if state.Offset != 110 {
		t.Fatalf("rearmed follow = %#v", state)
	}
}

func TestChatScrollBoundsExternalGeometryAndHydration(t *testing.T) {
	state := newChatScrollState()
	state.updateLayout(int(^uint(0)>>1), -100)
	if state.MaxOffset != maxChatScrollLines-1 || state.Offset != state.MaxOffset {
		t.Fatalf("bounded geometry = %#v", state)
	}
	state.userScrolled(-100)
	if state.Offset != 0 || state.FollowBottom || !state.shouldHydrateOlder(2) {
		t.Fatalf("bounded manual scroll = %#v", state)
	}
	state.userScrolled(state.MaxOffset)
	if state.shouldHydrateOlder(-1) {
		t.Fatalf("bottom requested hydration = %#v", state)
	}
}

func TestChatScrollFineWheelPageHydrationAndAnchorStability(t *testing.T) {
	state := newChatScrollState()
	state.updateLayout(200, 20)
	if offset := state.wheel(-1); offset != 177 || state.FollowBottom {
		t.Fatalf("fine wheel = %#v offset=%d", state, offset)
	}
	state.updateLayout(210, 20)
	if state.Offset != 177 {
		t.Fatalf("stream append dragged manual viewport: %#v", state)
	}
	state.updateLayoutPreservingAnchor(260, 20, 50)
	if state.Offset != 227 {
		t.Fatalf("hydration lost anchor: %#v", state)
	}
	state.userScrolled(10)
	if offset, hydrate := state.pageUp(2); offset != 0 || !hydrate {
		t.Fatalf("page up = offset %d hydrate=%v state=%#v", offset, hydrate, state)
	}
	state.userScrolled(state.MaxOffset)
	state.updateLayout(270, 20)
	if !state.FollowBottom || state.Offset != 250 {
		t.Fatalf("bottom rearm = %#v", state)
	}
}

func TestChatScrollThumbIsBounded(t *testing.T) {
	state := newChatScrollState()
	state.updateLayout(1_000, 100)
	state.userScrolled(450)
	start, size := state.thumb(20)
	if start != 9 || size != 2 {
		t.Fatalf("thumb = %d,%d", start, size)
	}
	state.userScrolled(state.MaxOffset)
	start, size = state.thumb(20)
	if start+size != 20 {
		t.Fatalf("bottom thumb = %d,%d", start, size)
	}
	if start, size = state.thumb(-1); start != 0 || size != 0 {
		t.Fatalf("negative track thumb = %d,%d", start, size)
	}
}
