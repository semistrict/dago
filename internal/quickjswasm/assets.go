// Package quickjswasm embeds the exact QuickJS-ng WASM artifacts shipped by
// quickjs-rs v0.2.5.
package quickjswasm

import _ "embed"

//go:generate go run ./generate.go

// Guest is the QuickJS execution guest.
//
//go:embed _guest.wasm
var Guest []byte

// TrackedGuest is Guest transformed with stores-only WAFL page tracking.
//
//go:embed _guest_tracked.wasm
var TrackedGuest []byte

// Transform is the OXC source-transform guest.
//
//go:embed _transform.wasm
var Transform []byte
