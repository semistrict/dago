// Package justbash defines the portable request boundary used to run a
// sandboxed just-bash shell beside a Go WebAssembly agent.
package justbash

import "context"

// Request contains only command execution data. Files are shared through the
// mounted filesystem and never serialized across this boundary.
type Request struct {
	Command             string `json:"command"`
	Cwd                 string `json:"cwd"`
	TimeoutMilliseconds int64  `json:"timeout_milliseconds"`
}

// Response contains bounded shell output and process-like status.
type Response struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Executor runs a command in a browser-owned sandbox.
type Executor func(context.Context, Request) (Response, error)
