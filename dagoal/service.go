package dagoal

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/internal/optionvalue"
)

// BudgetUpdate distinguishes keeping, setting, and clearing a token budget.
type BudgetUpdate struct {
	Update bool
	Value  *int64
}

// SetBudget returns a host mutation that sets a bounded token budget.
func SetBudget(value int64) BudgetUpdate { return BudgetUpdate{Update: true, Value: new(value)} }

// ClearBudget returns a host mutation that removes the token budget.
func ClearBudget() BudgetUpdate { return BudgetUpdate{Update: true} }

// SetRequest is a host-controlled goal mutation. Nil objective and status keep
// their current values; Budget.Update controls whether the budget changes.
type SetRequest struct {
	Objective *string
	Status    *Status
	Budget    BudgetUpdate
}

// Service performs host-controlled mutations while the owning agent is idle.
// Applications must not race these methods with a live invocation for the same
// thread.
type Service struct {
	agent   *dagent.Agent
	options Options
}

func NewService(agent *dagent.Agent, optionValues ...Options) *Service {
	if agent == nil {
		panic("goal service requires an agent")
	}
	options := optionvalue.Resolve("goal service", optionValues)
	return &Service{agent: agent, options: options.configured()}
}

func (service *Service) Get(ctx context.Context, config dacheckpoint.Config) (*Goal, error) {
	snapshot, err := service.agent.State(ctx, config)
	if err != nil {
		if errors.Is(err, dacheckpoint.ErrCheckpointMissing) {
			return nil, nil
		}
		return nil, fmt.Errorf("read goal: %w", err)
	}
	goal, _ := FromState(snapshot.State)
	return goal, nil
}

func (service *Service) Set(ctx context.Context, config dacheckpoint.Config, request SetRequest) (*Goal, error) {
	current, err := service.Get(ctx, config)
	if err != nil {
		return nil, err
	}
	if request.Status != nil && !request.Status.valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, *request.Status)
	}
	var objective string
	if request.Objective != nil {
		objective, err = validateObjective(*request.Objective)
		if err != nil {
			return nil, err
		}
	} else if current == nil {
		return nil, ErrNoGoal
	}
	if request.Budget.Update {
		if err := validateBudget(request.Budget.Value, service.options.MaxTokenBudget); err != nil {
			return nil, err
		}
	}
	now := service.options.Clock().UTC()
	goal := cloneGoal(current)
	if goal == nil || (goal.Status == StatusComplete && request.Objective != nil) {
		id, idErr := service.options.NewID()
		if idErr != nil {
			return nil, idErr
		}
		budget := request.Budget.Value
		if !request.Budget.Update && service.options.MaxTokenBudget > 0 {
			budget = new(service.options.MaxTokenBudget)
		}
		goal = &Goal{ID: id, Objective: objective, Status: StatusActive, TokenBudget: cloneBudget(budget), CreatedAt: now, UpdatedAt: now}
	} else {
		if request.Objective != nil {
			goal.Objective = objective
		}
		if request.Budget.Update {
			goal.TokenBudget = cloneBudget(request.Budget.Value)
		}
		goal.UpdatedAt = now
	}
	if request.Status != nil {
		goal.Status = *request.Status
	}
	if goal.Status == StatusActive && goal.TokenBudget != nil && goal.TokensUsed >= *goal.TokenBudget {
		goal.Status = StatusBudgetLimited
	}
	snapshot, err := service.agent.UpdateState(ctx, config, dastate.Values{StateKey: goalToState(goal)})
	if err != nil {
		return nil, fmt.Errorf("write goal: %w", err)
	}
	written, _ := FromState(snapshot.State)
	return written, nil
}

func (service *Service) Clear(ctx context.Context, config dacheckpoint.Config) (bool, error) {
	goal, err := service.Get(ctx, config)
	if err != nil || goal == nil {
		return false, err
	}
	if _, err := service.agent.UpdateState(ctx, config, dastate.Values{StateKey: goalState(nil)}); err != nil {
		return false, fmt.Errorf("clear goal: %w", err)
	}
	return true, nil
}
