package dacode

import (
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	maxThreadSelectorEntries = 1024
	maxThreadSelectorQuery   = 200
	maxThreadSelectorError   = 320
)

type threadSelectorAction uint8

const (
	threadSelectorNoAction threadSelectorAction = iota
	threadSelectorCancel
	threadSelectorResume
	threadSelectorConfirmDelete
	threadSelectorDelete
	threadSelectorPreferencesChanged
)

type threadSelectorFocus uint8

const (
	threadSelectorSearchFocus threadSelectorFocus = iota
	threadSelectorAgentFocus
	threadSelectorListFocus
	threadSelectorFocusCount
)

type threadSelectorPreferences struct {
	RelativeTime bool
	Agent        string
	AllAgents    bool
}

type threadSelectorOptions struct {
	Preferences threadSelectorPreferences
	Now         func() time.Time
}

func defaultThreadSelectorOptions() threadSelectorOptions {
	return threadSelectorOptions{
		Preferences: threadSelectorPreferences{RelativeTime: true, AllAgents: true},
		Now:         time.Now,
	}
}

type threadDeleteAuthorization struct {
	SelectorID     uint64
	Generation     uint64
	ThreadID       string
	CheckpointID   string
	ThreadRevision string
}

type threadSelectorResult struct {
	Action        threadSelectorAction
	Session       sessionInfo
	Authorization threadDeleteAuthorization
	Preferences   threadSelectorPreferences
}

// threadSelectorState is the UI-independent state machine for the /threads
// browser. Checkpoint metadata is bounded and terminal-safe before any filter
// or render path can use it. Deletion authorizations are bound to this exact
// selector instance, its current session generation, and the selected thread.
type threadSelectorState struct {
	sessions      []sessionInfo
	visible       []int
	selected      int
	query         string
	agent         string
	allAgents     bool
	currentThread string
	relativeTime  bool
	focus         threadSelectorFocus
	now           func() time.Time
	selectorID    uint64
	generation    uint64

	confirmingDelete *threadDeleteAuthorization
	deleting         *threadDeleteAuthorization
	deleteError      string
}

var nextThreadSelectorID atomic.Uint64

func newThreadSelectorState(sessions []sessionInfo, currentThread string) *threadSelectorState {
	return newThreadSelectorStateWithOptions(sessions, currentThread, defaultThreadSelectorOptions())
}

func newThreadSelectorStateWithOptions(sessions []sessionInfo, currentThread string, options threadSelectorOptions) *threadSelectorState {
	if options.Now == nil {
		options.Now = time.Now
	}
	agent := boundedTerminalText(options.Preferences.Agent, 128)
	allAgents := options.Preferences.AllAgents || agent == ""
	state := &threadSelectorState{
		sessions:      normalizeThreadSelectorSessions(sessions),
		currentThread: validThreadSelectorID(currentThread),
		relativeTime:  options.Preferences.RelativeTime,
		agent:         agent,
		allAgents:     allAgents,
		focus:         threadSelectorSearchFocus,
		now:           options.Now,
		selectorID:    nextThreadSelectorID.Add(1),
		generation:    1,
	}
	state.refilter()
	state.selectThread(state.currentThread)
	return state
}

func normalizeThreadSelectorSessions(sessions []sessionInfo) []sessionInfo {
	result := make([]sessionInfo, 0, min(len(sessions), maxThreadSelectorEntries))
	seen := make(map[string]bool, min(len(sessions), maxThreadSelectorEntries))
	for _, session := range sessions {
		if len(result) == maxThreadSelectorEntries {
			break
		}
		session.ThreadID = validThreadSelectorID(session.ThreadID)
		if session.ThreadID == "" || seen[session.ThreadID] {
			continue
		}
		seen[session.ThreadID] = true
		session.CheckpointID = boundedTerminalText(session.CheckpointID, 256)
		session.ThreadRevision = validThreadRevision(session.ThreadRevision)
		session.Preview = boundedTerminalText(session.Preview, 512)
		session.Directory = boundedTerminalText(session.Directory, 1024)
		session.Agent = boundedTerminalText(session.Agent, 128)
		session.Branch = boundedTerminalText(session.Branch, 256)
		session.CreatedAt = validThreadSelectorTime(session.CreatedAt)
		session.UpdatedAt = validThreadSelectorTime(session.UpdatedAt)
		session.MessageCount = boundedThreadSelectorCount(session.MessageCount)
		session.ContextTokens = boundedThreadSelectorCount(session.ContextTokens)
		result = append(result, session)
	}
	return result
}

func boundedThreadSelectorCount(value int) int {
	if value < 0 {
		return 0
	}
	return min(value, 1_000_000_000)
}

func validThreadSelectorTime(value time.Time) time.Time {
	if value.IsZero() || value.Year() < 1970 || value.Year() > 9999 {
		return time.Time{}
	}
	return value.Round(0)
}

func validThreadSelectorID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > 256 {
		return ""
	}
	for _, character := range value {
		if character > unicode.MaxASCII || unicode.IsControl(character) || unicode.IsSpace(character) {
			return ""
		}
	}
	return value
}

func validThreadRevision(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return ""
	}
	for _, character := range value {
		if character < '0' || character > '9' && (character < 'a' || character > 'f') {
			return ""
		}
	}
	return value
}

func boundedTerminalText(value string, limit int) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) || character == unicode.ReplacementChar {
			return ' '
		}
		return character
	}, value)
	value = unicodesecurity.RenderTerminalSafe(value)
	value = strings.Join(strings.Fields(value), " ")
	characters := []rune(value)
	if len(characters) > limit {
		characters = characters[:limit]
	}
	return string(characters)
}

func (state *threadSelectorState) preferences() threadSelectorPreferences {
	return threadSelectorPreferences{RelativeTime: state.relativeTime, Agent: state.agent, AllAgents: state.allAgents}
}

func (state *threadSelectorState) setQuery(value string) {
	state.query = boundedTerminalText(value, maxThreadSelectorQuery)
	state.cancelDeleteConfirmation()
	state.refilter()
}

func (state *threadSelectorState) setAgent(value string) {
	state.agent = boundedTerminalText(value, 128)
	state.allAgents = state.agent == ""
	state.cancelDeleteConfirmation()
	state.refilter()
}

func (state *threadSelectorState) setAllAgents() {
	state.allAgents = true
	state.cancelDeleteConfirmation()
	state.refilter()
}

func (state *threadSelectorState) toggleRelativeTime() { state.relativeTime = !state.relativeTime }

func (state *threadSelectorState) agentOptions() []string {
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, session := range state.sessions {
		if session.Agent != "" && !seen[session.Agent] {
			seen[session.Agent] = true
			result = append(result, session.Agent)
		}
	}
	sort.Strings(result)
	return result
}

func (state *threadSelectorState) cycleAgent(delta int) {
	options := append([]string{""}, state.agentOptions()...)
	current := 0
	if !state.allAgents {
		for index, option := range options {
			if option == state.agent {
				current = index
				break
			}
		}
	}
	selected := options[wrapIndex(current+delta, len(options))]
	state.agent = selected
	state.allAgents = selected == ""
	state.cancelDeleteConfirmation()
	state.refilter()
}

func (state *threadSelectorState) selectedSession() (sessionInfo, bool) {
	if state == nil || state.selected < 0 || state.selected >= len(state.visible) {
		return sessionInfo{}, false
	}
	return state.sessions[state.visible[state.selected]], true
}

func (state *threadSelectorState) move(delta int) {
	if len(state.visible) == 0 {
		state.selected = 0
		return
	}
	state.selected = wrapIndex(state.selected+delta, len(state.visible))
	state.cancelDeleteConfirmation()
}

func (state *threadSelectorState) handleKey(key string, pageHeight int) threadSelectorResult {
	if state == nil || state.deleting != nil {
		return threadSelectorResult{}
	}
	if state.confirmingDelete != nil {
		session, ok := state.sessionForAuthorization(*state.confirmingDelete)
		if !ok {
			state.cancelDeleteConfirmation()
			return threadSelectorResult{}
		}
		switch strings.ToLower(key) {
		case "enter", "y":
			authorization := *state.confirmingDelete
			state.confirmingDelete = nil
			state.deleting = &authorization
			state.deleteError = ""
			return threadSelectorResult{Action: threadSelectorDelete, Session: session, Authorization: authorization}
		case "esc", "n":
			state.cancelDeleteConfirmation()
		}
		return threadSelectorResult{}
	}
	rawKey := key
	key = strings.ToLower(key)
	switch key {
	case "esc":
		return threadSelectorResult{Action: threadSelectorCancel}
	case "tab":
		state.focus = threadSelectorFocus(wrapIndex(int(state.focus)+1, int(threadSelectorFocusCount)))
	case "shift+tab":
		state.focus = threadSelectorFocus(wrapIndex(int(state.focus)-1, int(threadSelectorFocusCount)))
	case "up":
		if state.focus == threadSelectorAgentFocus {
			state.cycleAgent(-1)
			return threadSelectorResult{Action: threadSelectorPreferencesChanged, Preferences: state.preferences()}
		} else {
			state.move(-1)
		}
	case "down":
		if state.focus == threadSelectorAgentFocus {
			state.cycleAgent(1)
			return threadSelectorResult{Action: threadSelectorPreferencesChanged, Preferences: state.preferences()}
		} else {
			state.move(1)
		}
	case "left":
		if state.focus == threadSelectorAgentFocus {
			state.cycleAgent(-1)
			return threadSelectorResult{Action: threadSelectorPreferencesChanged, Preferences: state.preferences()}
		}
	case "right":
		if state.focus == threadSelectorAgentFocus {
			state.cycleAgent(1)
			return threadSelectorResult{Action: threadSelectorPreferencesChanged, Preferences: state.preferences()}
		}
	case "pgup":
		state.move(-max(pageHeight, 1))
	case "pgdown":
		state.move(max(pageHeight, 1))
	case "ctrl+r":
		state.toggleRelativeTime()
		return threadSelectorResult{Action: threadSelectorPreferencesChanged, Preferences: state.preferences()}
	case "ctrl+d":
		session, ok := state.selectedSession()
		if ok {
			authorization := state.authorizeDelete(session)
			state.confirmingDelete = &authorization
			state.deleteError = ""
			return threadSelectorResult{Action: threadSelectorConfirmDelete, Session: session, Authorization: authorization}
		}
	case "enter":
		if state.focus == threadSelectorAgentFocus {
			state.cycleAgent(1)
			return threadSelectorResult{Action: threadSelectorPreferencesChanged, Preferences: state.preferences()}
		}
		session, ok := state.selectedSession()
		if ok {
			return threadSelectorResult{Action: threadSelectorResume, Session: session}
		}
	case " ":
		if state.focus == threadSelectorAgentFocus {
			state.setAllAgents()
			return threadSelectorResult{Action: threadSelectorPreferencesChanged, Preferences: state.preferences()}
		}
		state.appendQueryRune(' ')
	case "backspace":
		if state.focus == threadSelectorSearchFocus {
			characters := []rune(state.query)
			if len(characters) > 0 {
				state.setQuery(string(characters[:len(characters)-1]))
			}
		}
	default:
		characters := []rune(rawKey)
		if state.focus == threadSelectorSearchFocus && len(characters) == 1 && unicode.IsPrint(characters[0]) {
			state.appendQueryRune(characters[0])
		}
	}
	return threadSelectorResult{}
}

func (state *threadSelectorState) appendQueryRune(character rune) {
	if character != ' ' || state.query != "" {
		state.setQuery(state.query + string(character))
	}
}

func (state *threadSelectorState) authorizeDelete(session sessionInfo) threadDeleteAuthorization {
	return threadDeleteAuthorization{
		SelectorID: state.selectorID, Generation: state.generation,
		ThreadID: session.ThreadID, CheckpointID: session.CheckpointID, ThreadRevision: session.ThreadRevision,
	}
}

func (state *threadSelectorState) sessionForAuthorization(authorization threadDeleteAuthorization) (sessionInfo, bool) {
	if authorization.SelectorID != state.selectorID || authorization.Generation != state.generation || authorization.ThreadID == "" {
		return sessionInfo{}, false
	}
	for _, session := range state.sessions {
		if session.ThreadID == authorization.ThreadID && session.CheckpointID == authorization.CheckpointID && session.ThreadRevision == authorization.ThreadRevision {
			return session, true
		}
	}
	return sessionInfo{}, false
}

func (state *threadSelectorState) cancelDeleteConfirmation() { state.confirmingDelete = nil }

// finishDelete applies an asynchronous result only when it matches the exact
// in-flight authorization. Failures retain and reselect the row; stale results
// cannot delete a replacement row that happens to occupy the same position.
func (state *threadSelectorState) finishDelete(authorization threadDeleteAuthorization, err error) bool {
	if state == nil || state.deleting == nil || *state.deleting != authorization {
		return false
	}
	state.deleting = nil
	session, valid := state.sessionForAuthorization(authorization)
	if !valid {
		return false
	}
	if err != nil {
		state.deleteError = boundedTerminalText(err.Error(), maxThreadSelectorError)
		state.refilter()
		state.selectThread(session.ThreadID)
		return true
	}
	selected := state.selected
	replacement := make([]sessionInfo, 0, len(state.sessions)-1)
	for _, candidate := range state.sessions {
		if candidate.ThreadID != authorization.ThreadID {
			replacement = append(replacement, candidate)
		}
	}
	state.sessions = replacement
	state.visible = nil
	state.selected = 0
	state.generation++
	state.deleteError = ""
	state.refilter()
	if len(state.visible) != 0 {
		state.selected = min(selected, len(state.visible)-1)
	}
	return true
}

// replaceSessions establishes a new storage snapshot and invalidates every
// outstanding delete confirmation or completion from the previous snapshot.
func (state *threadSelectorState) replaceSessions(sessions []sessionInfo) {
	selectedThread := ""
	if selected, ok := state.selectedSession(); ok {
		selectedThread = selected.ThreadID
	}
	state.sessions = normalizeThreadSelectorSessions(sessions)
	state.visible = nil
	state.selected = 0
	state.generation++
	state.confirmingDelete = nil
	state.deleting = nil
	state.deleteError = ""
	state.refilter()
	state.selectThread(selectedThread)
}

func (state *threadSelectorState) refilter() {
	selectedThread := ""
	if session, ok := state.selectedSession(); ok {
		selectedThread = session.ThreadID
	}
	state.visible = state.visible[:0]
	query := strings.ToLower(state.query)
	agent := strings.ToLower(state.agent)
	for index, session := range state.sessions {
		if !state.allAgents && strings.ToLower(session.Agent) != agent {
			continue
		}
		haystack := strings.ToLower(strings.Join([]string{session.ThreadID, session.Agent, session.Branch, session.Directory, session.Preview}, " "))
		if query != "" && !threadSelectorSubsequence(query, haystack) {
			continue
		}
		state.visible = append(state.visible, index)
	}
	state.selected = 0
	state.selectThread(selectedThread)
}

func (state *threadSelectorState) selectThread(threadID string) {
	for visibleIndex, sessionIndex := range state.visible {
		if state.sessions[sessionIndex].ThreadID == threadID {
			state.selected = visibleIndex
			return
		}
	}
}

func threadSelectorSubsequence(needle, haystack string) bool {
	if needle == "" {
		return true
	}
	remaining := []rune(needle)
	for _, character := range haystack {
		if character == remaining[0] {
			remaining = remaining[1:]
			if len(remaining) == 0 {
				return true
			}
		}
	}
	return false
}
