package dago

import "time"

// Interpreter configures the agent-owned JavaScript code interpreter. Normal
// Go builds host an isolated QuickJS-ng WASM instance in Wazero. TinyGo builds
// exclude that implementation and reject configurations that enable it.
type Interpreter struct {
	Enabled          bool
	ToolName         string
	Timeout          time.Duration
	MemoryLimit      uint64
	StackLimit       uint64
	MaxStdoutChars   int
	MaxResultChars   int
	MaxSnapshotBytes int
	MaxPTCCalls      int
	// PTC is an allowlist of agent tool names exposed as async functions under
	// tools.*. Nil selects the read-only filesystem tools; an empty non-nil
	// slice disables programmatic tool calling.
	PTC []string
}
