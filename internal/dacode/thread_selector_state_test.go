package dacode

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestThreadSelectorFiltersNavigatesAndPreservesIdentity(t *testing.T) {
	state := newThreadSelectorState([]sessionInfo{
		{ThreadID: "one", Agent: "builder", Branch: "feature/alpha", Directory: "/work/alpha", Preview: "first request"},
		{ThreadID: "two", Agent: "reviewer", Branch: "security", Directory: "/work/beta", Preview: "security review"},
		{ThreadID: "three", Agent: "builder", Directory: "/work/gamma", Preview: "third request"},
	}, "two")
	if session, ok := state.selectedSession(); !ok || session.ThreadID != "two" {
		t.Fatalf("initial selection = %#v, %v", session, ok)
	}
	state.setQuery("screv")
	if len(state.visible) != 1 {
		t.Fatalf("fuzzy matches = %#v", state.visible)
	}
	if session, _ := state.selectedSession(); session.ThreadID != "two" {
		t.Fatalf("filtered selection = %#v", session)
	}
	state.setQuery("falpha")
	if session, ok := state.selectedSession(); !ok || session.ThreadID != "one" {
		t.Fatalf("branch search selection = %#v, %v", session, ok)
	}
	state.setQuery("")
	state.setAgent("builder")
	if state.allAgents || len(state.visible) != 2 {
		t.Fatalf("agent matches = %#v all=%v", state.visible, state.allAgents)
	}
	state.move(-1)
	if session, _ := state.selectedSession(); session.ThreadID != "three" {
		t.Fatalf("wrapped selection = %#v", session)
	}
	state.setAllAgents()
	if !state.allAgents || len(state.visible) != 3 {
		t.Fatalf("all-agent matches = %#v all=%v", state.visible, state.allAgents)
	}
	if options := state.agentOptions(); !slices.Equal(options, []string{"builder", "reviewer"}) {
		t.Fatalf("agent options = %#v", options)
	}
}

func TestThreadSelectorFocusAndPersistenceReadyPreferences(t *testing.T) {
	now := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	state := newThreadSelectorStateWithOptions([]sessionInfo{
		{ThreadID: "one", Agent: "builder"}, {ThreadID: "two", Agent: "reviewer"},
	}, "", threadSelectorOptions{
		Preferences: threadSelectorPreferences{Agent: "reviewer"},
		Now:         func() time.Time { return now },
	})
	if state.relativeTime || state.allAgents || len(state.visible) != 1 || state.agent != "reviewer" {
		t.Fatalf("restored preferences = %#v visible=%#v", state.preferences(), state.visible)
	}
	state.handleKey("tab", 5)
	if state.focus != threadSelectorAgentFocus {
		t.Fatalf("focus = %d", state.focus)
	}
	result := state.handleKey("right", 5)
	if result.Action != threadSelectorPreferencesChanged || !result.Preferences.AllAgents || len(state.visible) != 2 {
		t.Fatalf("agent cycle = %#v visible=%#v", result, state.visible)
	}
	result = state.handleKey("ctrl+r", 5)
	if result.Action != threadSelectorPreferencesChanged || !result.Preferences.RelativeTime {
		t.Fatalf("relative time preference = %#v", result)
	}
	state.handleKey("tab", 5)
	state.handleKey("tab", 5)
	if state.focus != threadSelectorSearchFocus {
		t.Fatalf("wrapped focus = %d", state.focus)
	}
	state.handleKey("shift+tab", 5)
	if state.focus != threadSelectorListFocus {
		t.Fatalf("reverse focus = %d", state.focus)
	}
	state.handleKey("x", 5)
	if state.query != "" {
		t.Fatalf("list focus changed query: %q", state.query)
	}
	state.handleKey("tab", 5)
	state.handleKey("A", 5)
	if state.query != "A" {
		t.Fatalf("search input lost case: %q", state.query)
	}

	spaceState := newThreadSelectorStateWithOptions([]sessionInfo{
		{ThreadID: "one", Agent: "builder"}, {ThreadID: "two", Agent: "reviewer"},
	}, "", threadSelectorOptions{Preferences: threadSelectorPreferences{Agent: "reviewer"}})
	spaceState.handleKey("tab", 5)
	result = spaceState.handleKey("space", 5)
	if result.Action != threadSelectorPreferencesChanged || !result.Preferences.AllAgents || len(spaceState.visible) != 2 {
		t.Fatalf("space agent reset = %#v visible=%#v", result, spaceState.visible)
	}
}

func TestThreadSelectorDeleteAuthorizationAndRollback(t *testing.T) {
	state := newThreadSelectorState([]sessionInfo{
		{ThreadID: "one", CheckpointID: "cp-1", ThreadRevision: strings.Repeat("1", 64)},
		{ThreadID: "two", CheckpointID: "cp-2", ThreadRevision: strings.Repeat("2", 64)},
		{ThreadID: "three", CheckpointID: "cp-3", ThreadRevision: strings.Repeat("3", 64)},
	}, "two")
	confirmation := state.handleKey("ctrl+d", 5)
	if confirmation.Action != threadSelectorConfirmDelete || confirmation.Authorization.ThreadID != "two" || confirmation.Authorization.CheckpointID != "cp-2" || state.confirmingDelete == nil {
		t.Fatalf("confirmation = %#v state=%#v", confirmation, state)
	}
	if confirmation.Authorization.SelectorID != state.selectorID || confirmation.Authorization.Generation != state.generation {
		t.Fatalf("authorization not bound to selector snapshot: %#v", confirmation.Authorization)
	}
	state.handleKey("esc", 5)
	if state.confirmingDelete != nil {
		t.Fatal("Esc did not cancel deletion")
	}

	state.handleKey("ctrl+d", 5)
	request := state.handleKey("enter", 5)
	if request.Action != threadSelectorDelete || state.deleting == nil || state.confirmingDelete != nil {
		t.Fatalf("delete request = %#v state=%#v", request, state)
	}
	if got := state.handleKey("down", 5); got.Action != threadSelectorNoAction {
		t.Fatalf("input while deleting = %#v", got)
	}
	wrongCheckpoint := request.Authorization
	wrongCheckpoint.CheckpointID = "cp-other"
	if state.finishDelete(wrongCheckpoint, nil) {
		t.Fatal("mismatched checkpoint authorization was applied")
	}
	failure := errors.New("backend\x1b[31m\nfailed")
	if !state.finishDelete(request.Authorization, failure) {
		t.Fatal("matching failure was ignored")
	}
	if session, ok := state.selectedSession(); !ok || session.ThreadID != "two" || len(state.sessions) != 3 {
		t.Fatalf("failure did not retain and reselect exact row: %#v, %v sessions=%#v", session, ok, state.sessions)
	}
	if state.deleteError != "backend [31m failed" || state.deleting != nil {
		t.Fatalf("failure state = error %q deleting %#v", state.deleteError, state.deleting)
	}

	state.handleKey("ctrl+d", 5)
	request = state.handleKey("y", 5)
	if !state.finishDelete(request.Authorization, nil) {
		t.Fatal("matching success was ignored")
	}
	if got := threadIDs(state.sessions); !slices.Equal(got, []string{"one", "three"}) {
		t.Fatalf("success deleted wrong rows: %#v", got)
	}
	if session, ok := state.selectedSession(); !ok || session.ThreadID != "three" {
		t.Fatalf("post-delete selection = %#v, %v", session, ok)
	}
}

func TestThreadSelectorDeleteCompletionRejectsStaleSnapshots(t *testing.T) {
	state := newThreadSelectorState([]sessionInfo{{ThreadID: "target", ThreadRevision: strings.Repeat("a", 64)}, {ThreadID: "keep", ThreadRevision: strings.Repeat("b", 64)}}, "target")
	state.handleKey("ctrl+d", 4)
	request := state.handleKey("enter", 4)
	state.replaceSessions([]sessionInfo{{ThreadID: "target", Preview: "replacement", ThreadRevision: strings.Repeat("c", 64)}, {ThreadID: "new", ThreadRevision: strings.Repeat("d", 64)}})
	if state.finishDelete(request.Authorization, nil) {
		t.Fatal("stale completion was applied")
	}
	if got := threadIDs(state.sessions); !slices.Equal(got, []string{"target", "new"}) {
		t.Fatalf("replacement snapshot changed: %#v", got)
	}

	other := newThreadSelectorState([]sessionInfo{{ThreadID: "target", ThreadRevision: strings.Repeat("e", 64)}}, "")
	other.handleKey("ctrl+d", 4)
	otherRequest := other.handleKey("enter", 4)
	state.deleting = &otherRequest.Authorization
	if state.finishDelete(otherRequest.Authorization, nil) {
		t.Fatal("foreign selector authorization was applied")
	}
}

func TestThreadSelectorBoundsAndSanitizesExternalMetadata(t *testing.T) {
	sessions := make([]sessionInfo, maxThreadSelectorEntries+20)
	for index := range sessions {
		sessions[index] = sessionInfo{
			ThreadID:      fmt.Sprintf("thread-%d", index),
			CheckpointID:  "checkpoint\x1b",
			Preview:       "unsafe\x1b[31m\npreview\u202e",
			Directory:     "/tmp/unsafe\x1b",
			Agent:         "agent\x07",
			Branch:        strings.Repeat("b", 300) + "\u2066",
			CreatedAt:     time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt:     time.Date(1969, 1, 1, 0, 0, 0, 0, time.UTC),
			MessageCount:  -1,
			ContextTokens: 2_000_000_000,
		}
	}
	state := newThreadSelectorState(sessions, "")
	if len(state.sessions) != maxThreadSelectorEntries {
		t.Fatalf("sessions = %d", len(state.sessions))
	}
	for _, session := range state.sessions {
		for _, value := range []string{session.ThreadID, session.CheckpointID, session.Preview, session.Directory, session.Agent, session.Branch} {
			if strings.ContainsAny(value, "\x1b\n\r\a\u202e\u2066") {
				t.Fatalf("unsafe metadata survived: %q", value)
			}
		}
		if len([]rune(session.Branch)) > 256 {
			t.Fatalf("branch length = %d", len([]rune(session.Branch)))
		}
		if !session.CreatedAt.IsZero() || !session.UpdatedAt.IsZero() {
			t.Fatalf("invalid times survived: %v %v", session.CreatedAt, session.UpdatedAt)
		}
		if session.MessageCount != 0 || session.ContextTokens != 1_000_000_000 {
			t.Fatalf("counts = %d, %d", session.MessageCount, session.ContextTokens)
		}
	}
	state.setQuery(strings.Repeat("q", maxThreadSelectorQuery+50))
	if len([]rune(state.query)) != maxThreadSelectorQuery {
		t.Fatalf("query length = %d", len([]rune(state.query)))
	}
}

func TestRenderThreadSelectorResponsivePaginationAndDeterministicTime(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	sessions := make([]sessionInfo, 30)
	for index := range sessions {
		sessions[index] = sessionInfo{
			ThreadID: fmt.Sprintf("thread-%02d", index), Agent: "builder", Branch: "feature/thread-list",
			Directory: "/work/project", Preview: "implement selector", MessageCount: index,
			CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-30 * time.Minute),
		}
	}
	var nowCalls atomic.Int32
	state := newThreadSelectorStateWithOptions(sessions, "thread-20", threadSelectorOptions{
		Preferences: threadSelectorPreferences{RelativeTime: true, AllAgents: true},
		Now: func() time.Time {
			nowCalls.Add(1)
			return now
		},
	})
	view := ansi.Strip(renderThreadSelector(state, 120, 16, asciiUIGlyphs))
	for _, expected := range []string{"CREATED", "UPDATED", "BRANCH", "2h ago", "30m ago", "Showing 20-22 of 30", "thread-20*"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("missing %q:\n%s", expected, view)
		}
	}
	if nowCalls.Load() != 1 {
		t.Fatalf("Now calls = %d", nowCalls.Load())
	}
	for _, line := range strings.Split(view, "\n") {
		if ansi.StringWidth(line) > 120 {
			t.Fatalf("line width = %d: %q", ansi.StringWidth(line), line)
		}
	}

	state.toggleRelativeTime()
	compact := ansi.Strip(renderThreadSelector(state, 50, 16, asciiUIGlyphs))
	if !strings.Contains(compact, "2026-") || strings.Contains(compact, "BRANCH") || strings.Contains(compact, "CREATED") {
		t.Fatalf("compact absolute view:\n%s", compact)
	}
	for _, line := range strings.Split(compact, "\n") {
		if ansi.StringWidth(line) > 50 {
			t.Fatalf("compact line width = %d: %q", ansi.StringWidth(line), line)
		}
	}
}

func TestFormatThreadSelectorTime(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value time.Time
		want  string
	}{
		{name: "unknown", want: "-"},
		{name: "now", value: now.Add(-30 * time.Second), want: "now"},
		{name: "minutes", value: now.Add(-15 * time.Minute), want: "15m ago"},
		{name: "hours", value: now.Add(-3 * time.Hour), want: "3h ago"},
		{name: "days", value: now.Add(-2 * 24 * time.Hour), want: "2d ago"},
		{name: "weeks", value: now.Add(-15 * 24 * time.Hour), want: "2w ago"},
		{name: "months", value: now.Add(-90 * 24 * time.Hour), want: "3mo ago"},
		{name: "years", value: now.Add(-730 * 24 * time.Hour), want: "2y ago"},
		{name: "future", value: now.Add(3 * time.Hour), want: "in 3h"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatThreadSelectorTime(test.value, true, now); got != test.want {
				t.Fatalf("time = %q, want %q", got, test.want)
			}
		})
	}
	if got := formatThreadSelectorTime(now, false, time.Time{}); got != "2026-08-17" {
		t.Fatalf("absolute time = %q", got)
	}
}

type recordingThreadDeleter struct {
	mu            sync.Mutex
	threadIDs     []string
	checkpointIDs []string
	revisions     []string
	err           error
}

func (deleter *recordingThreadDeleter) DeleteSession(ctx context.Context, threadID, checkpointID, revision string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deleter.mu.Lock()
	defer deleter.mu.Unlock()
	deleter.threadIDs = append(deleter.threadIDs, threadID)
	deleter.checkpointIDs = append(deleter.checkpointIDs, checkpointID)
	deleter.revisions = append(deleter.revisions, revision)
	return deleter.err
}

func TestDeleteSelectedThreadUsesExactAuthorizedIdentity(t *testing.T) {
	deleter := &recordingThreadDeleter{}
	authorization := threadDeleteAuthorization{SelectorID: 7, Generation: 11, ThreadID: "exact-thread", CheckpointID: "exact-checkpoint", ThreadRevision: strings.Repeat("a", 64)}
	message, ok := deleteSelectedThread(context.Background(), deleter, authorization)().(threadDeleteCompletedMsg)
	if !ok || message.Err != nil || message.Authorization != authorization {
		t.Fatalf("message = %#v, %v", message, ok)
	}
	if !slices.Equal(deleter.threadIDs, []string{"exact-thread"}) || !slices.Equal(deleter.checkpointIDs, []string{"exact-checkpoint"}) {
		t.Fatalf("deleted IDs = %#v checkpoints=%#v", deleter.threadIDs, deleter.checkpointIDs)
	}
	if !slices.Equal(deleter.revisions, []string{strings.Repeat("a", 64)}) {
		t.Fatalf("deleted revisions = %#v", deleter.revisions)
	}
	message = deleteSelectedThread(context.Background(), nil, authorization)().(threadDeleteCompletedMsg)
	if message.Err == nil || !strings.Contains(message.Err.Error(), "unavailable") {
		t.Fatalf("nil deleter error = %v", message.Err)
	}
	invalid := authorization
	invalid.ThreadID = "unsafe thread"
	message = deleteSelectedThread(context.Background(), deleter, invalid)().(threadDeleteCompletedMsg)
	if message.Err == nil || !strings.Contains(message.Err.Error(), "invalid") || len(deleter.threadIDs) != 1 {
		t.Fatalf("invalid authorization message=%#v calls=%#v", message, deleter.threadIDs)
	}
}

func TestDeleteSelectedThreadCommandsAreRaceSafe(t *testing.T) {
	deleter := &recordingThreadDeleter{}
	const count = 32
	var wait sync.WaitGroup
	wait.Add(count)
	for index := range count {
		go func() {
			defer wait.Done()
			authorization := threadDeleteAuthorization{SelectorID: 1, Generation: 1, ThreadID: fmt.Sprintf("thread-%02d", index)}
			message := deleteSelectedThread(context.Background(), deleter, authorization)().(threadDeleteCompletedMsg)
			if message.Err != nil {
				t.Errorf("delete %d: %v", index, message.Err)
			}
		}()
	}
	wait.Wait()
	deleter.mu.Lock()
	defer deleter.mu.Unlock()
	if len(deleter.threadIDs) != count {
		t.Fatalf("delete calls = %d", len(deleter.threadIDs))
	}
}

func threadIDs(sessions []sessionInfo) []string {
	result := make([]string, len(sessions))
	for index, session := range sessions {
		result[index] = session.ThreadID
	}
	return result
}
