// Package dagoal provides durable, provider-neutral goals for dago agents.
package dagoal

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
)

const StateKey = "goal"

var (
	ErrNoGoal         = errors.New("thread has no goal")
	ErrUnfinishedGoal = errors.New("thread has an unfinished goal")
	ErrInvalidGoal    = errors.New("invalid goal")
	ErrInvalidStatus  = errors.New("invalid goal status")
	ErrInvalidBudget  = errors.New("invalid goal token budget")
)

// Status is the persisted lifecycle state of one thread goal.
type Status string

const (
	StatusActive        Status = "active"
	StatusPaused        Status = "paused"
	StatusBlocked       Status = "blocked"
	StatusUsageLimited  Status = "usage_limited"
	StatusBudgetLimited Status = "budget_limited"
	StatusComplete      Status = "complete"
)

func (status Status) valid() bool {
	switch status {
	case StatusActive, StatusPaused, StatusBlocked, StatusUsageLimited, StatusBudgetLimited, StatusComplete:
		return true
	default:
		return false
	}
}

// Goal is the stable JSON representation stored in agent checkpoint state.
type Goal struct {
	ID              string    `json:"id"`
	Objective       string    `json:"objective"`
	Status          Status    `json:"status"`
	TokenBudget     *int64    `json:"token_budget,omitempty"`
	TokensUsed      int64     `json:"tokens_used"`
	TimeUsedSeconds int64     `json:"time_used_seconds"`
	CreatedAt       time.Time `json:"created_at,omitzero"`
	UpdatedAt       time.Time `json:"updated_at,omitzero"`
}

// RemainingTokens returns the bounded remaining budget, or nil for an
// unbudgeted goal.
func (goal Goal) RemainingTokens() *int64 {
	if goal.TokenBudget == nil {
		return nil
	}
	remaining := max(*goal.TokenBudget-goal.TokensUsed, 0)
	return &remaining
}

// Actionable reports whether automatic work should continue.
func (goal Goal) Actionable() bool { return goal.Status == StatusActive }

// Options configures goal middleware and host-side mutations.
type Options struct {
	MaxTokenBudget int64
	Clock          func() time.Time
	NewID          func() (string, error)
}

func (options Options) configured() Options {
	if options.MaxTokenBudget < 0 {
		panic("goal maximum token budget cannot be negative")
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	if options.NewID == nil {
		options.NewID = newGoalID
	}
	return options
}

func newGoalID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate goal id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func validateObjective(objective string) (string, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return "", fmt.Errorf("%w: objective is required", ErrInvalidGoal)
	}
	return objective, nil
}

func validateBudget(budget *int64, maximum int64) error {
	if budget == nil {
		return nil
	}
	if *budget <= 0 {
		return fmt.Errorf("%w: budget must be positive", ErrInvalidBudget)
	}
	if maximum > 0 && *budget > maximum {
		return fmt.Errorf("%w: budget %d exceeds maximum %d", ErrInvalidBudget, *budget, maximum)
	}
	return nil
}

func cloneGoal(goal *Goal) *Goal {
	if goal == nil {
		return nil
	}
	copy := *goal
	if goal.TokenBudget != nil {
		copy.TokenBudget = new(*goal.TokenBudget)
	}
	return &copy
}

type goalState map[string]any

func goalToState(goal *Goal) goalState {
	if goal == nil {
		return nil
	}
	state := goalState{
		"id": goal.ID, "objective": goal.Objective, "status": string(goal.Status),
		"tokens_used": goal.TokensUsed, "time_used_seconds": goal.TimeUsedSeconds,
	}
	if goal.TokenBudget != nil {
		state["token_budget"] = *goal.TokenBudget
	}
	if !goal.CreatedAt.IsZero() {
		state["created_at"] = goal.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if !goal.UpdatedAt.IsZero() {
		state["updated_at"] = goal.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return state
}

func cloneGoalState(state goalState) goalState {
	if state == nil {
		return nil
	}
	copy := make(goalState, len(state))
	for key, value := range state {
		copy[key] = value
	}
	return copy
}

// FromState decodes live or checkpoint-restored goal state. Present is true
// when the goal channel exists, including when it has explicitly been cleared.
func FromState(values dastate.Values) (goal *Goal, present bool) {
	value, present := values[StateKey]
	if !present || value == nil {
		return nil, present
	}
	switch typed := value.(type) {
	case goalState:
		if len(typed) == 0 {
			return nil, true
		}
	case map[string]any:
		if len(typed) == 0 {
			return nil, true
		}
	}
	if typed, ok := value.(*Goal); ok {
		return validDecodedGoal(cloneGoal(typed)), true
	}
	if typed, ok := value.(Goal); ok {
		return validDecodedGoal(cloneGoal(&typed)), true
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, true
	}
	var decoded Goal
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, true
	}
	return validDecodedGoal(&decoded), true
}

func validDecodedGoal(goal *Goal) *Goal {
	if goal == nil || goal.ID == "" || strings.TrimSpace(goal.Objective) == "" || !goal.Status.valid() {
		return nil
	}
	if goal.TokenBudget != nil && *goal.TokenBudget <= 0 {
		return nil
	}
	return goal
}

// ContinuationMessage builds the hidden host turn used to keep an active goal
// moving after the thread becomes idle.
func ContinuationMessage(goal Goal) damessage.Message {
	remaining := "unbounded"
	if value := goal.RemainingTokens(); value != nil {
		remaining = fmt.Sprintf("%d", *value)
	}
	message := damessage.Human(fmt.Sprintf(`<goal_continuation>
Continue working autonomously toward the active thread goal.

- Objective: %s
- Tokens used: %d
- Remaining token budget: %s

Use get_goal if you need the authoritative snapshot. Do not repeat completed work. Keep working while meaningful progress remains. Call update_goal with status "complete" only when the objective is fully achieved, or "blocked" only when its strict blocker conditions are satisfied.
</goal_continuation>`, escapeXML(goal.Objective), goal.TokensUsed, remaining))
	message.Metadata = map[string]json.RawMessage{"dago_goal_control": json.RawMessage(`true`)}
	return message
}

func escapeXML(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	return strings.ReplaceAll(value, ">", "&gt;")
}
