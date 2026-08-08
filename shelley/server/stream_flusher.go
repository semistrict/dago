package server

import (
	"sync"
	"time"

	"shelley.exe.dev/llm"
)

// streamFlusher batches LLM stream deltas and flushes them periodically.
// Anthropic's SSE stream emits hundreds of tiny text_delta events per second.
// Broadcasting each one individually overwhelms the subpub channel (buffer=10),
// causing subscriber disconnections. Instead, we accumulate deltas and flush
// the combined text every interval (e.g., 50ms), yielding ~20 updates/second.
type streamFlusher struct {
	cm       *ConversationManager
	interval time.Duration

	mu      sync.Mutex
	buf     string // accumulated text since last flush
	index   int    // content block index of accumulated text
	timer   *time.Timer
	running bool
}

// nextSeq returns the next monotonically increasing sequence number. The
// counter lives on the ConversationManager so it survives loop resets and is
// truly per-conversation. Safe to call without holding sf.mu.
func (sf *streamFlusher) nextSeq() int64 {
	return sf.cm.streamDeltaSeq.Add(1)
}

func newStreamFlusher(cm *ConversationManager, interval time.Duration) *streamFlusher {
	return &streamFlusher{
		cm:       cm,
		interval: interval,
	}
}

// Push adds a stream delta to the buffer and schedules a flush.
func (sf *streamFlusher) Push(delta llm.StreamDelta) {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	if delta.Type == "text" {
		sf.buf += delta.Text
		sf.index = delta.Index
	} else {
		// For non-text deltas (thinking, etc.), broadcast immediately.
		delta.Seq = sf.nextSeq()
		sf.cm.broadcastStream(StreamResponse{
			StreamDelta: &delta,
		})
		return
	}

	if !sf.running {
		sf.running = true
		sf.timer = time.AfterFunc(sf.interval, sf.flush)
	}
}

func (sf *streamFlusher) flush() {
	sf.mu.Lock()
	text := sf.buf
	idx := sf.index
	sf.buf = ""
	sf.running = false
	if sf.timer != nil {
		sf.timer.Stop()
		sf.timer = nil
	}
	// Assign the seq while still holding sf.mu so its order matches the order
	// text was accumulated. (nextSeq itself is atomic and doesn't require the
	// lock; the lock here only orders assignment relative to concurrent
	// Push calls.)
	var seq int64
	if text != "" {
		seq = sf.nextSeq()
	}
	sf.mu.Unlock()

	if text != "" {
		sf.cm.broadcastStream(StreamResponse{
			StreamDelta: &llm.StreamDelta{
				Type:  "text",
				Text:  text,
				Index: idx,
				Seq:   seq,
			},
		})
	}
}

// Flush forces any buffered text to be broadcast immediately.
// Call this before recording the final assistant message to ensure
// deltas reach the UI before the full message replaces them.
func (sf *streamFlusher) Flush() {
	sf.flush()
}
