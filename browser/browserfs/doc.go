// Package browserfs implements a bounded, just-in-time browser workspace for
// Go WebAssembly agents.
//
// The implementation is available when building for js/wasm. It indexes only
// directory metadata at connection time, reads file bodies on demand, writes
// directly through File System Access API handles, and stores virtual files as
// independent records through a caller-provided Store.
package browserfs
