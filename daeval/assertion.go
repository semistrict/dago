package daeval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/semistrict/dago/damessage"
)

// Check is a deterministic assertion over an agent trajectory.
type Check interface {
	Evaluate(Trajectory) (bool, string)
}

type checkFunc func(Trajectory) (bool, string)

func (check checkFunc) Evaluate(trajectory Trajectory) (bool, string) {
	return check(trajectory)
}

// FinalTextContains requires the final assistant text to contain text. Common
// zero-width formatting characters are ignored on both sides of the match.
func FinalTextContains(text string) Check {
	return textCheck(text, false, true)
}

// FinalTextContainsFold is the case-insensitive form of FinalTextContains.
func FinalTextContainsFold(text string) Check {
	return textCheck(text, true, true)
}

// FinalTextContainsAny requires the final assistant text to contain at least
// one candidate. Common zero-width formatting characters are ignored.
func FinalTextContainsAny(texts ...string) Check {
	if len(texts) == 0 {
		panic("at least one final-text candidate is required")
	}
	for _, text := range texts {
		requireText(text)
	}
	candidates := append([]string(nil), texts...)
	return checkFunc(func(trajectory Trajectory) (bool, string) {
		answer := stripZeroWidth(trajectory.Answer())
		for _, text := range candidates {
			if strings.Contains(answer, stripZeroWidth(text)) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("final text contains none of %q", candidates)
	})
}

// FinalTextExcludes requires the final assistant text not to contain text.
func FinalTextExcludes(text string) Check {
	return textCheck(text, false, false)
}

// FinalTextExcludesFold is the case-insensitive form of FinalTextExcludes.
func FinalTextExcludesFold(text string) Check {
	return textCheck(text, true, false)
}

func textCheck(text string, fold, contains bool) Check {
	requireText(text)
	return checkFunc(func(trajectory Trajectory) (bool, string) {
		answer := stripZeroWidth(trajectory.Answer())
		needle := stripZeroWidth(text)
		if fold {
			answer = strings.ToLower(answer)
			needle = strings.ToLower(needle)
		}
		matched := strings.Contains(answer, needle)
		if contains == matched {
			return true, ""
		}
		if contains {
			return false, fmt.Sprintf("final text does not contain %q", text)
		}
		return false, fmt.Sprintf("final text unexpectedly contains %q", text)
	})
}

// FinalTextMinLength requires at least length Unicode code points after
// trimming surrounding whitespace.
func FinalTextMinLength(length int) Check {
	if length < 0 {
		panic("minimum final-text length must not be negative")
	}
	return checkFunc(func(trajectory Trajectory) (bool, string) {
		actual := utf8.RuneCountInString(strings.TrimSpace(stripZeroWidth(trajectory.Answer())))
		if actual >= length {
			return true, ""
		}
		return false, fmt.Sprintf("final text length is %d, want at least %d", actual, length)
	})
}

// FileEquals requires path to contain exactly content.
func FileEquals(path, content string) Check {
	requirePath(path)
	return checkFunc(func(trajectory Trajectory) (bool, string) {
		actual, exists := trajectory.Files[path]
		if exists && actual == content {
			return true, ""
		}
		if !exists {
			return false, fmt.Sprintf("file %q is absent", path)
		}
		return false, fmt.Sprintf("file %q content differs", path)
	})
}

// FileContains requires path to contain text.
func FileContains(path, text string) Check {
	requirePath(path)
	requireText(text)
	return checkFunc(func(trajectory Trajectory) (bool, string) {
		actual, exists := trajectory.Files[path]
		if exists && strings.Contains(actual, text) {
			return true, ""
		}
		if !exists {
			return false, fmt.Sprintf("file %q is absent", path)
		}
		return false, fmt.Sprintf("file %q does not contain %q", path, text)
	})
}

// FileExcludes requires path not to contain text. An absent file passes.
func FileExcludes(path, text string) Check {
	requirePath(path)
	requireText(text)
	return checkFunc(func(trajectory Trajectory) (bool, string) {
		if !strings.Contains(trajectory.Files[path], text) {
			return true, ""
		}
		return false, fmt.Sprintf("file %q unexpectedly contains %q", path, text)
	})
}

// FileAbsent requires path not to exist.
func FileAbsent(path string) Check {
	requirePath(path)
	return checkFunc(func(trajectory Trajectory) (bool, string) {
		if _, exists := trajectory.Files[path]; !exists {
			return true, ""
		}
		return false, fmt.Sprintf("file %q still exists", path)
	})
}

// ToolCheck selects a tool call by name, optional one-based step, and optional
// JSON arguments. Values are immutable: selector methods return a copy.
type ToolCheck struct {
	name      string
	want      bool
	step      int
	arguments map[string]any
	exact     bool
}

// ToolCalled requires a call to name.
func ToolCalled(name string) ToolCheck {
	if strings.TrimSpace(name) == "" {
		panic("tool name is required")
	}
	return ToolCheck{name: name, want: true}
}

// ToolNotCalled requires no call to name.
func ToolNotCalled(name string) ToolCheck {
	check := ToolCalled(name)
	check.want = false
	return check
}

// AtStep restricts the selector to a one-based trajectory step.
func (check ToolCheck) AtStep(step int) ToolCheck {
	if step <= 0 {
		panic("tool-call step must be positive")
	}
	check.step = step
	return check
}

// WithArguments requires the call arguments to contain every supplied field.
func (check ToolCheck) WithArguments(arguments map[string]any) ToolCheck {
	check.arguments = cloneArguments(arguments)
	check.exact = false
	return check
}

// WithExactArguments requires the call arguments to equal the supplied object.
func (check ToolCheck) WithExactArguments(arguments map[string]any) ToolCheck {
	check.arguments = cloneArguments(arguments)
	check.exact = true
	return check
}

func (check ToolCheck) Evaluate(trajectory Trajectory) (bool, string) {
	if check.name == "" {
		return false, "tool check is not configured"
	}
	if check.step > len(trajectory.Steps) {
		return false, fmt.Sprintf("cannot check step %d; trajectory has %d step(s)", check.step, len(trajectory.Steps))
	}
	matches := 0
	for index, step := range trajectory.Steps {
		if check.step != 0 && index+1 != check.step {
			continue
		}
		for _, call := range step.Action.ToolCalls {
			if toolCallMatches(call, check) {
				matches++
			}
		}
	}
	if (matches > 0) == check.want {
		return true, ""
	}
	step := ""
	if check.step != 0 {
		step = fmt.Sprintf(" in step %d", check.step)
	}
	if check.want {
		return false, fmt.Sprintf("no %q tool call matched%s", check.name, step)
	}
	return false, fmt.Sprintf("found %d forbidden %q tool call(s)%s", matches, check.name, step)
}

func toolCallMatches(call damessage.ToolCall, check ToolCheck) bool {
	if call.Name != check.name {
		return false
	}
	if check.arguments == nil {
		return true
	}
	var actual map[string]any
	if len(call.Arguments) == 0 || json.Unmarshal(call.Arguments, &actual) != nil {
		return false
	}
	if check.exact {
		return reflect.DeepEqual(actual, check.arguments)
	}
	for key, expected := range check.arguments {
		actualValue, exists := actual[key]
		if !exists || !reflect.DeepEqual(actualValue, expected) {
			return false
		}
	}
	return true
}

type stepCountCheck struct{ expected int }

// StepCount expects exactly expected assistant steps. Put it in
// Evaluation.Expectations when the count should be diagnostic rather than a
// correctness gate.
func StepCount(expected int) Check {
	if expected < 0 {
		panic("expected step count must not be negative")
	}
	return stepCountCheck{expected: expected}
}

func (check stepCountCheck) Evaluate(trajectory Trajectory) (bool, string) {
	actual := len(trajectory.Steps)
	if actual == check.expected {
		return true, ""
	}
	return false, fmt.Sprintf("got %d assistant steps, want %d", actual, check.expected)
}

type toolCallCountCheck struct{ expected int }

// ToolCallCount expects exactly expected tool-call requests. Put it in
// Evaluation.Expectations when the count should be diagnostic rather than a
// correctness gate.
func ToolCallCount(expected int) Check {
	if expected < 0 {
		panic("expected tool-call count must not be negative")
	}
	return toolCallCountCheck{expected: expected}
}

func (check toolCallCountCheck) Evaluate(trajectory Trajectory) (bool, string) {
	actual := trajectory.ToolCallCount()
	if actual == check.expected {
		return true, ""
	}
	return false, fmt.Sprintf("got %d tool-call requests, want %d", actual, check.expected)
}

// MaxToolCallCount expects no more than maximum tool-call requests.
func MaxToolCallCount(maximum int) Check {
	if maximum < 0 {
		panic("maximum tool-call count must not be negative")
	}
	return checkFunc(func(trajectory Trajectory) (bool, string) {
		actual := trajectory.ToolCallCount()
		if actual <= maximum {
			return true, ""
		}
		return false, fmt.Sprintf("got %d tool-call requests, want at most %d", actual, maximum)
	})
}

func stripZeroWidth(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\ufeff':
			return -1
		default:
			return r
		}
	}, value)
}

func requirePath(path string) {
	if strings.TrimSpace(path) == "" {
		panic("file path is required")
	}
}

func requireText(text string) {
	if text == "" {
		panic("comparison text is required")
	}
}

func cloneArguments(arguments map[string]any) map[string]any {
	if arguments == nil {
		return nil
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		panic(fmt.Sprintf("tool-call arguments must be JSON-compatible: %v", err))
	}
	cloned := map[string]any{}
	if err := json.NewDecoder(bytes.NewReader(encoded)).Decode(&cloned); err != nil {
		panic(fmt.Sprintf("clone tool-call arguments: %v", err))
	}
	return cloned
}
