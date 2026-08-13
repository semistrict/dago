package dagoal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

const goalSystemPrompt = `## Thread goals

Goals are durable, explicitly requested objectives that may span multiple turns.

- Do not create a goal for an ordinary task. Use create_goal only when the user or higher-priority instructions explicitly request a goal.
- If a state notice below reports an active goal, keep making meaningful progress toward it across turns.
- Use get_goal to inspect authoritative status and usage.
- Use update_goal only to mark a goal complete or genuinely blocked. The host owns pause, resume, limit, and clear transitions.`

const goalRunKey = "__goal_run"

type goalRun map[string]string

const (
	goalRunIDKey      = "goal_id"
	goalRunStartedKey = "started_at"
)

type createGoalInput struct {
	Objective   string `json:"objective" jsonschema:"description=The concrete objective to pursue."`
	TokenBudget *int64 `json:"token_budget,omitempty" jsonschema:"description=Positive token budget; omit unless explicitly requested."`
}

type updateGoalInput struct {
	Status Status `json:"status" jsonschema:"enum=complete|blocked,description=Terminal status to apply."`
}

type goalResponse struct {
	Goal                   *Goal  `json:"goal"`
	RemainingTokens        *int64 `json:"remaining_tokens"`
	CompletionBudgetReport string `json:"completion_budget_report,omitempty"`
}

// Middleware adds durable goal state, model-visible goal tools, active-goal
// context, and per-model-call token and elapsed-time accounting.
func Middleware(options Options) dagent.Middleware {
	options = options.configured()
	get := datool.MustNew("get_goal", "Get the current goal for this thread, including status, token and elapsed-time usage, and remaining token budget.",
		func(ctx context.Context, _ struct{}) (datool.Result, error) {
			runtime, ok := datool.RuntimeFromContext(ctx)
			if !ok {
				return datool.Result{}, errors.New("goal tool runtime is unavailable")
			}
			goal, _ := goalFromReader(runtime.State)
			return responseResult(goalResponseFor(goal, false), nil), nil
		})
	create := datool.MustNew("create_goal", `Create a goal only when explicitly requested by the user or system/developer instructions; do not infer goals from ordinary tasks.
Set token_budget only when an explicit token budget is requested. This fails while an unfinished goal exists.`,
		func(ctx context.Context, input createGoalInput) (datool.Result, error) {
			runtime, ok := datool.RuntimeFromContext(ctx)
			if !ok {
				return datool.Result{}, errors.New("goal tool runtime is unavailable")
			}
			current, _ := goalFromReader(runtime.State)
			if current != nil && current.Status != StatusComplete {
				return datool.Result{}, ErrUnfinishedGoal
			}
			objective, err := validateObjective(input.Objective)
			if err != nil {
				return datool.Result{}, err
			}
			budget := input.TokenBudget
			if budget == nil && options.MaxTokenBudget > 0 {
				budget = new(options.MaxTokenBudget)
			}
			if err := validateBudget(budget, options.MaxTokenBudget); err != nil {
				return datool.Result{}, err
			}
			id, err := options.NewID()
			if err != nil {
				return datool.Result{}, err
			}
			now := options.Clock().UTC()
			goal := &Goal{ID: id, Objective: objective, Status: StatusActive, TokenBudget: cloneBudget(budget), CreatedAt: now, UpdatedAt: now}
			accountCreationCall(goal, runtime.State)
			if goal.TokenBudget != nil && goal.TokensUsed >= *goal.TokenBudget {
				goal.Status = StatusBudgetLimited
			}
			return responseResult(goalResponseFor(goal, false), map[string]any{StateKey: goalToState(goal)}), nil
		})
	update := datool.MustNew("update_goal", `Update the existing goal only to mark it complete or genuinely blocked.
Set complete only when the full objective is achieved and no required work remains. Set blocked only after the same blocking condition has repeated for at least three consecutive goal turns and meaningful progress requires user input or an external change. The host controls pause, resume, budget-limited, usage-limited, and clear transitions.`,
		func(ctx context.Context, input updateGoalInput) (datool.Result, error) {
			runtime, ok := datool.RuntimeFromContext(ctx)
			if !ok {
				return datool.Result{}, errors.New("goal tool runtime is unavailable")
			}
			goal, _ := goalFromReader(runtime.State)
			if goal == nil {
				return datool.Result{}, ErrNoGoal
			}
			if input.Status != StatusComplete && input.Status != StatusBlocked {
				return datool.Result{}, fmt.Errorf("%w: update_goal accepts only complete or blocked", ErrInvalidStatus)
			}
			goal.Status = input.Status
			goal.UpdatedAt = options.Clock().UTC()
			return responseResult(goalResponseFor(goal, input.Status == StatusComplete), map[string]any{StateKey: goalToState(goal)}), nil
		})

	return dagent.Middleware{
		Name: "goals", SerializedName: "GoalMiddleware",
		Fields: map[string]dagent.StateField{
			StateKey: dagent.Field(dagent.FieldSpec[goalState]{
				Kind: dagent.FieldLast, Contract: "dago.goal.v1", Clone: cloneGoalState,
			}),
			goalRunKey: dagent.Field(dagent.FieldSpec[goalRun]{
				Kind: dagent.FieldEphemeral, Contract: "dago.goal.run.v1", Private: true,
				Clone: cloneGoalRun,
			}),
		},
		Tools: []datool.Tool{get, create, update},
		BeforeAgent: func(_ context.Context, values dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
			goal, _ := goalFromValues(values)
			if goal == nil || goal.Status != StatusActive {
				return nil, nil
			}
			return dastate.Values{goalRunKey: goalRun{
				goalRunIDKey: goal.ID, goalRunStartedKey: options.Clock().UTC().Format(time.RFC3339Nano),
			}}, nil
		},
		WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
			goal, _ := goalFromValues(request.State)
			appendGoalSystem(&request, goal)
			response, err := next(ctx, request)
			if err != nil || goal == nil || goal.Status != StatusActive {
				return response, err
			}
			updated := cloneGoal(goal)
			usage := damessage.AggregateUsage(response.Messages).Add(trailingToolUsage(request.Messages))
			updated.TokensUsed += int64(usage.TotalTokens)
			if updated.TokenBudget != nil && updated.TokensUsed >= *updated.TokenBudget {
				updated.Status = StatusBudgetLimited
			}
			updated.UpdatedAt = options.Clock().UTC()
			if response.Update == nil {
				response.Update = dastate.Values{}
			}
			response.Update[StateKey] = goalToState(updated)
			return response, nil
		},
		AfterAgent: func(_ context.Context, values dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
			run, ok := values[goalRunKey].(goalRun)
			if !ok || run[goalRunIDKey] == "" {
				return nil, nil
			}
			goal, _ := goalFromValues(values)
			if goal == nil || goal.ID != run[goalRunIDKey] {
				return nil, nil
			}
			startedAt, err := time.Parse(time.RFC3339Nano, run[goalRunStartedKey])
			if err != nil {
				return nil, nil
			}
			if elapsed := options.Clock().Sub(startedAt) / time.Second; elapsed > 0 {
				goal.TimeUsedSeconds += int64(elapsed)
				goal.UpdatedAt = options.Clock().UTC()
				return dastate.Values{StateKey: goalToState(goal)}, nil
			}
			return nil, nil
		},
	}
}

func cloneGoalRun(run goalRun) goalRun {
	if run == nil {
		return nil
	}
	copy := make(goalRun, len(run))
	for key, value := range run {
		copy[key] = value
	}
	return copy
}

func goalFromReader(state datool.StateReader) (*Goal, bool) {
	if state == nil {
		return nil, false
	}
	value, present := state.Get(StateKey)
	if !present || value == nil {
		return nil, present
	}
	if state, ok := value.(map[string]any); ok && len(state) == 0 {
		return nil, true
	}
	if state, ok := value.(goalState); ok && len(state) == 0 {
		return nil, true
	}
	decoded, ok := datool.StateAs[Goal](state, StateKey)
	if ok {
		return validDecodedGoal(cloneGoal(&decoded)), true
	}
	pointer, ok := datool.StateAs[*Goal](state, StateKey)
	return validDecodedGoal(cloneGoal(pointer)), ok
}

func trailingToolUsage(messages []damessage.Message) damessage.Usage {
	var usage damessage.Usage
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Role != damessage.RoleTool {
			break
		}
		usage = usage.Add(damessage.AggregateUsage(messages[index : index+1]))
	}
	return usage
}

func goalFromValues(values dastate.Values) (*Goal, bool) { return FromState(values) }

func cloneBudget(value *int64) *int64 {
	if value == nil {
		return nil
	}
	return new(*value)
}

func responseResult(response goalResponse, update map[string]any) datool.Result {
	encoded, err := json.Marshal(response)
	if err != nil {
		panic(err)
	}
	result := datool.TextResult(string(encoded))
	result.Structured = encoded
	result.Update = update
	return result
}

func goalResponseFor(goal *Goal, completion bool) goalResponse {
	response := goalResponse{Goal: cloneGoal(goal)}
	if goal != nil {
		response.RemainingTokens = goal.RemainingTokens()
		if completion && goal.TokenBudget != nil {
			response.CompletionBudgetReport = fmt.Sprintf("Goal completed after using %d of %d tokens.", goal.TokensUsed, *goal.TokenBudget)
		}
	}
	return response
}

func accountCreationCall(goal *Goal, state datool.StateReader) {
	if state == nil {
		return
	}
	messages, ok := datool.StateAs[[]damessage.Message](state, dagent.MessagesKey)
	if !ok {
		return
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != damessage.RoleAssistant {
			continue
		}
		usage := damessage.AggregateUsage(messages[index : index+1])
		goal.TokensUsed += int64(usage.TotalTokens)
		if usage.FinishedAt.After(usage.StartedAt) {
			goal.TimeUsedSeconds += int64(usage.FinishedAt.Sub(usage.StartedAt) / time.Second)
		}
		return
	}
}

func appendGoalSystem(request *dagent.ModelRequest, goal *Goal) {
	text := goalSystemPrompt
	if goal != nil {
		remaining := "unbounded"
		if value := goal.RemainingTokens(); value != nil {
			remaining = fmt.Sprintf("%d", *value)
		}
		text += fmt.Sprintf("\n\n<thread_goal>\nObjective: %s\nStatus: %s\nTokens used: %d\nRemaining token budget: %s\n</thread_goal>", escapeXML(goal.Objective), goal.Status, goal.TokensUsed, remaining)
	}
	if request.SystemMessage == nil {
		message := damessage.System(text)
		request.SystemMessage = &message
		return
	}
	copy := request.SystemMessage.Clone()
	copy.Content = append(copy.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: "\n\n" + strings.TrimSpace(text)})
	request.SystemMessage = &copy
}
