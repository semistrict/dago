package dacode

import "time"

const defaultConfirmationWindow = 3 * time.Second

type confirmationKind string

const (
	confirmQuit       confirmationKind = "quit"
	confirmClearInput confirmationKind = "clear_input"
	confirmDelete     confirmationKind = "delete"
)

type confirmationArms struct {
	deadlines map[confirmationKind]time.Time
	armedAt   map[confirmationKind]time.Time
}

func newConfirmationArms() *confirmationArms {
	return &confirmationArms{
		deadlines: make(map[confirmationKind]time.Time),
		armedAt:   make(map[confirmationKind]time.Time),
	}
}

func (arms *confirmationArms) press(kind confirmationKind, now time.Time) bool {
	arms.requireInitialized()
	if !validConfirmationKind(kind) || now.IsZero() {
		panic("dacode: confirmation press is invalid")
	}
	if deadline, exists := arms.deadlines[kind]; exists {
		if now.Before(arms.armedAt[kind]) {
			// A regressed clock cannot start or satisfy a confirmation window.
			arms.disarm(kind)
			return false
		}
		if now.Before(deadline) {
			arms.disarm(kind)
			return true
		}
	}
	arms.intervene(kind)
	arms.deadlines[kind] = now.Add(defaultConfirmationWindow)
	arms.armedAt[kind] = now
	return false
}

// intervene disarms confirmations with lower priority than an action that is
// now taking precedence. Delete confirmation outranks draft clearing, which
// outranks an idle quit arm. Same- and higher-priority arms remain independent.
func (arms *confirmationArms) intervene(kind confirmationKind) {
	arms.requireInitialized()
	if !validConfirmationKind(kind) {
		panic("dacode: confirmation kind is invalid")
	}
	priority := confirmationPriority(kind)
	for armed := range arms.deadlines {
		if confirmationPriority(armed) < priority {
			delete(arms.deadlines, armed)
			delete(arms.armedAt, armed)
		}
	}
}

func confirmationPriority(kind confirmationKind) int {
	switch kind {
	case confirmQuit:
		return 1
	case confirmClearInput:
		return 2
	case confirmDelete:
		return 3
	default:
		return 0
	}
}

func (arms *confirmationArms) disarm(kind confirmationKind) {
	arms.requireInitialized()
	if !validConfirmationKind(kind) {
		panic("dacode: confirmation kind is invalid")
	}
	delete(arms.deadlines, kind)
	delete(arms.armedAt, kind)
}

func (arms *confirmationArms) expire(now time.Time) {
	arms.requireInitialized()
	if now.IsZero() {
		panic("dacode: confirmation clock is required")
	}
	for kind, deadline := range arms.deadlines {
		if !now.Before(deadline) {
			delete(arms.deadlines, kind)
			delete(arms.armedAt, kind)
		}
	}
}

func (arms *confirmationArms) requireInitialized() {
	if arms == nil || arms.deadlines == nil || arms.armedAt == nil {
		panic("dacode: initialized confirmation state is required")
	}
}

func validConfirmationKind(kind confirmationKind) bool {
	switch kind {
	case confirmQuit, confirmClearInput, confirmDelete:
		return true
	default:
		return false
	}
}
