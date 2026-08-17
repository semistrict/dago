// Package quickjswasm embeds the QuickJS-ng execution guest shipped by
// quickjs-rs v0.2.5 and dago's source-controlled fork of its transform guest.
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

// Transform is the OXC source-transform guest with workflow-module support.
//
//go:embed _transform.wasm
var Transform []byte
