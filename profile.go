package dago

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/tool"
)

type ProfileKind string

const (
	ProfileHarness  ProfileKind = "harness"
	ProfileProvider ProfileKind = "provider"
)

// Profile is a composable construction overlay. Later profiles win for scalar
// and tool-description values; slices append in declaration order.
type Profile struct {
	Name              string
	Kind              ProfileKind
	SystemPrompt      string
	ToolDescriptions  map[string]string
	ExcludeTools      []string
	Middleware        []agent.Middleware
	ExcludeMiddleware []string
}

var profileRegistry = struct {
	sync.RWMutex
	values map[string]Profile
}{values: map[string]Profile{}}

// RegisterProfile installs a named immutable profile. Duplicate registration is
// rejected so package initialization order cannot silently change behavior.
func RegisterProfile(profile Profile) error {
	if strings.TrimSpace(profile.Name) == "" {
		return fmt.Errorf("profile name is required")
	}
	if profile.Kind != ProfileHarness && profile.Kind != ProfileProvider {
		return fmt.Errorf("profile %q has invalid kind %q", profile.Name, profile.Kind)
	}
	profile = cloneProfile(profile)
	profileRegistry.Lock()
	defer profileRegistry.Unlock()
	if _, exists := profileRegistry.values[profile.Name]; exists {
		return fmt.Errorf("profile %q is already registered", profile.Name)
	}
	profileRegistry.values[profile.Name] = profile
	return nil
}

func LookupProfile(name string) (Profile, bool) {
	profileRegistry.RLock()
	defer profileRegistry.RUnlock()
	profile, exists := profileRegistry.values[name]
	return cloneProfile(profile), exists
}

func RegisteredProfiles() []string {
	profileRegistry.RLock()
	defer profileRegistry.RUnlock()
	names := make([]string, 0, len(profileRegistry.values))
	for name := range profileRegistry.values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func MergeProfiles(profiles ...Profile) Profile {
	result := Profile{ToolDescriptions: map[string]string{}}
	for _, profile := range profiles {
		if profile.Name != "" {
			result.Name = profile.Name
		}
		if profile.Kind != "" {
			result.Kind = profile.Kind
		}
		if profile.SystemPrompt != "" {
			if result.SystemPrompt != "" {
				result.SystemPrompt += "\n\n"
			}
			result.SystemPrompt += profile.SystemPrompt
		}
		for name, description := range profile.ToolDescriptions {
			result.ToolDescriptions[name] = description
		}
		result.ExcludeTools = appendUnique(result.ExcludeTools, profile.ExcludeTools...)
		result.Middleware = append(result.Middleware, profile.Middleware...)
		result.ExcludeMiddleware = appendUnique(result.ExcludeMiddleware, profile.ExcludeMiddleware...)
	}
	return result
}

func resolveProfiles(names []string, inline []Profile) (Profile, error) {
	values := make([]Profile, 0, len(names)+len(inline))
	for _, name := range names {
		profile, exists := LookupProfile(name)
		if !exists {
			return Profile{}, fmt.Errorf("unknown profile %q", name)
		}
		values = append(values, profile)
	}
	values = append(values, inline...)
	return MergeProfiles(values...), nil
}

func cloneProfile(profile Profile) Profile {
	copy := profile
	copy.ToolDescriptions = make(map[string]string, len(profile.ToolDescriptions))
	for name, description := range profile.ToolDescriptions {
		copy.ToolDescriptions[name] = description
	}
	copy.ExcludeTools = append([]string(nil), profile.ExcludeTools...)
	copy.Middleware = append([]agent.Middleware(nil), profile.Middleware...)
	copy.ExcludeMiddleware = append([]string(nil), profile.ExcludeMiddleware...)
	return copy
}

func appendUnique(values []string, additions ...string) []string {
	seen := make(map[string]bool, len(values)+len(additions))
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		if value != "" && !seen[value] {
			values = append(values, value)
			seen[value] = true
		}
	}
	return values
}

type profileTool struct {
	tool.Tool
	description string
}

func (wrapped profileTool) Definition() tool.Definition {
	definition := wrapped.Tool.Definition()
	definition.Description = wrapped.description
	return definition
}

func applyToolProfile(values []tool.Tool, descriptions map[string]string, excluded map[string]bool) []tool.Tool {
	result := make([]tool.Tool, 0, len(values))
	for _, executable := range values {
		definition := executable.Definition()
		if excluded[definition.Name] {
			continue
		}
		if description := descriptions[definition.Name]; description != "" {
			executable = profileTool{Tool: executable, description: description}
		}
		result = append(result, executable)
	}
	return result
}

const RubricResultKey = "rubric_result"

type RubricCriterion struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Weight      float64 `json:"weight,omitempty"`
}

type RubricOptions struct {
	Model           model.Chat
	Criteria        []RubricCriterion
	Prompt          string
	FallbackOnError bool
}

// RubricMiddleware performs an opt-in, provider-neutral structured grading call
// after the primary agent has completed.
func RubricMiddleware(options RubricOptions) (agent.Middleware, error) {
	if options.Model == nil || len(options.Criteria) == 0 {
		return agent.Middleware{}, fmt.Errorf("rubric model and criteria are required")
	}
	for _, criterion := range options.Criteria {
		if criterion.Name == "" || criterion.Description == "" || criterion.Weight < 0 {
			return agent.Middleware{}, fmt.Errorf("rubric criteria require name, description, and non-negative weight")
		}
	}
	if options.Prompt == "" {
		options.Prompt = "Grade the assistant response against every rubric criterion. Return concise evidence and a score from 0 to 1 for each criterion, plus an overall score from 0 to 1."
	}
	schema := json.RawMessage(`{"type":"object","properties":{"scores":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"score":{"type":"number","minimum":0,"maximum":1},"evidence":{"type":"string"}},"required":["name","score","evidence"],"additionalProperties":false}},"overall":{"type":"number","minimum":0,"maximum":1}},"required":["scores","overall"],"additionalProperties":false}`)
	return agent.Middleware{
		Name: "rubric",
		Fields: map[string]agent.StateField{RubricResultKey: {
			Kind: agent.FieldLast, Contract: "dago.rubric.v1", Clone: func(value any) any {
				if raw, ok := value.(json.RawMessage); ok {
					return append(json.RawMessage(nil), raw...)
				}
				return value
			},
		}},
		AfterAgent: func(ctx context.Context, values state.Values, _ agent.Runtime) (state.Values, error) {
			messages, err := profileMessages(values[agent.MessagesKey])
			if err != nil {
				return nil, err
			}
			criteria, _ := json.Marshal(options.Criteria)
			request := []message.Message{
				message.System(options.Prompt),
				message.Human("Criteria:\n" + string(criteria) + "\n\nConversation:\n" + renderHistory(messages)),
			}
			response, err := options.Model.Invoke(ctx, model.Request{Messages: request, ResponseFormat: &model.ResponseFormat{Name: "rubric_grade", Description: "Rubric scores", Schema: schema, Strict: true}})
			if err != nil {
				if options.FallbackOnError {
					fallback, _ := json.Marshal(map[string]any{"scores": []any{}, "overall": 0, "error": err.Error()})
					return state.Values{RubricResultKey: json.RawMessage(fallback)}, nil
				}
				return nil, err
			}
			if !json.Valid(response.Structured) {
				if options.FallbackOnError {
					return state.Values{RubricResultKey: json.RawMessage(`{"scores":[],"overall":0,"error":"grader returned invalid structured output"}`)}, nil
				}
				return nil, fmt.Errorf("rubric grader returned invalid structured output")
			}
			return state.Values{RubricResultKey: append(json.RawMessage(nil), response.Structured...)}, nil
		},
	}, nil
}

func profileMessages(value any) ([]message.Message, error) {
	switch typed := value.(type) {
	case []message.Message:
		return typed, nil
	case []any:
		result := make([]message.Message, len(typed))
		for index, item := range typed {
			messageValue, ok := item.(message.Message)
			if !ok {
				return nil, fmt.Errorf("messages[%d] has type %T", index, item)
			}
			result[index] = messageValue
		}
		return result, nil
	default:
		return nil, fmt.Errorf("messages have type %T", value)
	}
}
