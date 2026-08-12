package daserver

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type persistedState struct {
	Assistants map[string]*Assistant `json:"assistants"`
	Threads    map[string]*Thread    `json:"threads"`
	Runs       map[string]*Run       `json:"runs"`
}

type streamEvent struct {
	ID    int64
	Event string
	Data  any
}

type eventLog struct {
	mu     sync.Mutex
	events []streamEvent
	notify chan struct{}
	done   bool
}

func newEventLog() *eventLog { return &eventLog{notify: make(chan struct{})} }

func (log *eventLog) append(event string, data any) {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.done {
		return
	}
	log.events = append(log.events, streamEvent{ID: int64(len(log.events)), Event: event, Data: data})
	close(log.notify)
	log.notify = make(chan struct{})
}

func (log *eventLog) finish() {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.done {
		return
	}
	log.done = true
	close(log.notify)
}

func (log *eventLog) snapshot(index int) ([]streamEvent, bool, <-chan struct{}) {
	log.mu.Lock()
	defer log.mu.Unlock()
	if index < 0 {
		index = 0
	}
	if index > len(log.events) {
		index = len(log.events)
	}
	items := append([]streamEvent(nil), log.events[index:]...)
	return items, log.done, log.notify
}

func (server *Server) loadState() error {
	if server.statePath == "" {
		return nil
	}
	data, err := os.ReadFile(server.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("load Agent Server state: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode Agent Server state: %w", err)
	}
	if state.Assistants != nil {
		server.assistants = state.Assistants
	}
	if state.Threads != nil {
		server.threads = state.Threads
	}
	if state.Runs != nil {
		server.runs = state.Runs
	}
	for _, run := range server.runs {
		if run.Status == "pending" || run.Status == "running" {
			run.Status = "error"
			run.UpdatedAt = server.timestamp()
		}
	}
	for _, thread := range server.threads {
		if thread.Status == "busy" {
			thread.Status = "error"
			thread.UpdatedAt = server.timestamp()
		}
	}
	return nil
}

func (server *Server) persistLocked() error {
	if server.statePath == "" {
		return nil
	}
	data, err := json.MarshalIndent(persistedState{
		Assistants: server.assistants, Threads: server.threads, Runs: server.runs,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Agent Server state: %w", err)
	}
	directory := filepath.Dir(server.statePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Agent Server state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".state-*")
	if err != nil {
		return fmt.Errorf("create Agent Server state file: %w", err)
	}
	temporaryName := temporary.Name()
	cleanup := func() { _ = os.Remove(temporaryName) }
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		cleanup()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		cleanup()
		return fmt.Errorf("write Agent Server state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		cleanup()
		return fmt.Errorf("sync Agent Server state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close Agent Server state: %w", err)
	}
	if err := os.Rename(temporaryName, server.statePath); err != nil {
		cleanup()
		return fmt.Errorf("replace Agent Server state: %w", err)
	}
	return nil
}

func newUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return formatUUID(value), nil
}

func graphUUID(graphID string) string {
	// Same UUIDv5 namespace used by LangGraph Agent Server for default graph
	// assistants.
	namespace := [16]byte{0x6b, 0xa7, 0xb8, 0x21, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	hash := sha1.New()
	_, _ = hash.Write(namespace[:])
	_, _ = hash.Write([]byte(graphID))
	sum := hash.Sum(nil)
	var value [16]byte
	copy(value[:], sum)
	value[6] = (value[6] & 0x0f) | 0x50
	value[8] = (value[8] & 0x3f) | 0x80
	return formatUUID(value)
}

func formatUUID(value [16]byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func mapMatches(values, filter map[string]any) bool {
	for key, wanted := range filter {
		left, _ := json.Marshal(values[key])
		right, _ := json.Marshal(wanted)
		if string(left) != string(right) {
			return false
		}
	}
	return true
}

func paginate[T any](items []T, offset, limit int) []T {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 10
	}
	if offset >= len(items) {
		return []T{}
	}
	items = items[offset:]
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func sortedAssistants(values map[string]*Assistant) []*Assistant {
	result := make([]*Assistant, 0, len(values))
	for _, assistant := range values {
		copy := cloneJSON(*assistant)
		result = append(result, &copy)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt != result[j].CreatedAt {
			return result[i].CreatedAt > result[j].CreatedAt
		}
		return result[i].AssistantID < result[j].AssistantID
	})
	return result
}

func sortedThreads(values map[string]*Thread) []*Thread {
	result := make([]*Thread, 0, len(values))
	for _, thread := range values {
		copy := cloneJSON(*thread)
		result = append(result, &copy)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt != result[j].CreatedAt {
			return result[i].CreatedAt > result[j].CreatedAt
		}
		return result[i].ThreadID < result[j].ThreadID
	})
	return result
}

func sortedRuns(values map[string]*Run, threadID string) []*Run {
	result := make([]*Run, 0)
	for _, run := range values {
		if threadID != "" && run.ThreadID != threadID {
			continue
		}
		copy := cloneJSON(*run)
		result = append(result, &copy)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt != result[j].CreatedAt {
			return result[i].CreatedAt > result[j].CreatedAt
		}
		return result[i].RunID < result[j].RunID
	})
	return result
}

func cloneJSON[T any](value T) T {
	data, _ := json.Marshal(value)
	var result T
	_ = json.Unmarshal(data, &result)
	return result
}

func containsFold(value, fragment string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(fragment))
}
