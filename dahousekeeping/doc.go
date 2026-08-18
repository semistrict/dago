// Package dahousekeeping provides bounded, application-owned startup chores.
//
// It contains three independent facilities: an idempotent legacy-state mover,
// a Go module dependency-floor checker, and a structured debug trace handler.
// None of them performs network access or discovers ambient authority.
package dahousekeeping
