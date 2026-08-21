package dago

import "time"

// Interpreter configures the agent-owned JavaScript code interpreter. Normal
// Go builds host an isolated QuickJS-ng WASM instance in Wazero. TinyGo builds
// exclude that implementation and reject interpreter configurations.
type Interpreter struct {
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
	// PTCTransparency emits programmatic tool calls through the ordinary tool
	// lifecycle stream so user interfaces and protocol adapters can render
	// them like model-originated calls. It does not add the calls to model
	// history or route them through tool-call middleware.
	PTCTransparency bool
}
