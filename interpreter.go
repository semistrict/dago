package dago

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/internal/quickjs"
)

const interpreterSnapshotKey = "_js_interpreter_snapshot"

// Interpreter configures the agent-owned JavaScript code interpreter. It uses
// an isolated QuickJS-ng WASM instance hosted by Wazero's portable interpreter.
type Interpreter struct {
	Enabled          bool
	ToolName         string
	Timeout          time.Duration
	MemoryLimit      uint64
	StackLimit       uint64
	MaxStdoutChars   int
	MaxResultChars   int
	MaxSnapshotBytes int
	MaxPTCCalls      int
	// PTC is an allowlist of agent tool names exposed as async functions under
	// tools.*. Nil selects the read-only filesystem tools; an empty non-nil
	// slice disables programmatic tool calling.
	PTC []string
}

type interpreterRuntime struct {
	options Interpreter
	mu      sync.RWMutex
	tools   map[string]map[string]datool.Tool
	active  map[string]bool
}

type ptcCallBudgetError struct {
	Limit, Attempted int64
	Function         string
}

func (e *ptcCallBudgetError) Error() string {
	return fmt.Sprintf("PTC call budget exceeded: limit=%d attempted=%d function=tools.%s", e.Limit, e.Attempted, jsIdentifier(e.Function))
}

type interpreterInput struct {
	Code string `json:"code" description:"JavaScript to evaluate. Top-level await is supported and bindings persist for this thread."`
}

func newInterpreter(options Interpreter) (dagent.Middleware, error) {
	if !options.Enabled {
		return dagent.Middleware{}, nil
	}
	if options.ToolName == "" {
		options.ToolName = "js_eval"
	}
	if options.Timeout < 0 || options.MaxStdoutChars < 0 || options.MaxResultChars < 0 || options.MaxSnapshotBytes < 0 || options.MaxPTCCalls < 0 {
		return dagent.Middleware{}, fmt.Errorf("interpreter limits cannot be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = 5 * time.Second
	}
	if options.MemoryLimit == 0 {
		options.MemoryLimit = 64 << 20
	}
	if options.StackLimit == 0 {
		options.StackLimit = 512 << 10
	}
	if options.MaxStdoutChars <= 0 {
		options.MaxStdoutChars = 20_000
	}
	if options.MaxResultChars <= 0 {
		options.MaxResultChars = 20_000
	}
	if options.MaxSnapshotBytes <= 0 {
		if options.MemoryLimit > uint64(^uint(0)>>1) {
			return dagent.Middleware{}, fmt.Errorf("interpreter memory limit exceeds platform capacity")
		}
		options.MaxSnapshotBytes = int(options.MemoryLimit)
	}
	if options.MaxPTCCalls <= 0 {
		options.MaxPTCCalls = 32
	}
	if options.PTC == nil {
		options.PTC = []string{"read_file", "glob", "grep"}
	}
	runtime := &interpreterRuntime{options: options, tools: map[string]map[string]datool.Tool{}, active: map[string]bool{}}
	tool := datool.MustNew(options.ToolName, `Evaluate JavaScript in a persistent, isolated QuickJS REPL. Use it for calculations, data transformation, iteration, and parallel calls to the documented tools.* functions.`, runtime.evaluate)
	middleware := dagent.Middleware{
		Name: "code_interpreter", SerializedName: "CodeInterpreterMiddleware",
		Tools:  []datool.Tool{tool},
		Fields: map[string]dagent.StateField{interpreterSnapshotKey: interpreterSnapshotField(options.MaxSnapshotBytes)},
	}
	middleware.WrapModelCall = func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
		selected, err := runtime.selectTools(request.Tools)
		if err != nil {
			return dagent.ModelResponse{}, err
		}
		key := interpreterThreadKey(request.Runtime.Config.ThreadID, request.Runtime.TaskID)
		runtime.mu.Lock()
		runtime.tools[key] = selected
		runtime.mu.Unlock()
		appendSystem(&request, interpreterPrompt(options.ToolName, selected))
		return next(ctx, request)
	}
	middleware.AfterAgent = func(_ context.Context, _ dastate.Values, runtimeContext dagent.Runtime) (dastate.Values, error) {
		key := interpreterThreadKey(runtimeContext.Config.ThreadID, runtimeContext.TaskID)
		runtime.mu.Lock()
		delete(runtime.tools, key)
		runtime.mu.Unlock()
		return nil, nil
	}
	return middleware, nil
}

func (runtime *interpreterRuntime) selectTools(tools []datool.Tool) (map[string]datool.Tool, error) {
	allowed := make(map[string]bool, len(runtime.options.PTC))
	for _, name := range runtime.options.PTC {
		allowed[name] = true
	}
	selected := map[string]datool.Tool{}
	identifiers := map[string]string{}
	for _, tool := range tools {
		name := tool.Definition().Name
		if name != runtime.options.ToolName && allowed[name] {
			identifier := jsIdentifier(name)
			if previous, exists := identifiers[identifier]; exists && previous != name {
				return nil, fmt.Errorf("PTC tools %q and %q map to the same JavaScript name %q", previous, name, identifier)
			}
			identifiers[identifier] = name
			selected[name] = tool
		}
	}
	return selected, nil
}

func (runtime *interpreterRuntime) evaluate(ctx context.Context, input interpreterInput) (datool.Result, error) {
	toolRuntime, ok := datool.RuntimeFromContext(ctx)
	if !ok {
		return datool.Result{}, fmt.Errorf("JavaScript interpreter runtime is unavailable")
	}
	key := interpreterThreadKey(toolRuntime.ThreadID, toolRuntime.TaskID)
	runtime.mu.Lock()
	if runtime.active[key] {
		runtime.mu.Unlock()
		return datool.Result{}, fmt.Errorf("JavaScript interpreter is already evaluating for this thread")
	}
	runtime.active[key] = true
	defer func() {
		runtime.mu.Lock()
		delete(runtime.active, key)
		runtime.mu.Unlock()
	}()
	selected := make(map[string]datool.Tool, len(runtime.tools[key]))
	for name, tool := range runtime.tools[key] {
		selected[name] = tool
	}
	runtime.mu.Unlock()
	var snapshot []byte
	if toolRuntime.State != nil {
		stored, ok := toolRuntime.State.Get(interpreterSnapshotKey)
		if ok {
			snapshot = materializedInterpreterSnapshot(stored)
		}
	}
	if len(snapshot) > 0 && quickjs.ValidateSnapshot(snapshot, runtime.options.MaxSnapshotBytes) != nil {
		snapshot = nil
	}
	var calls atomic.Int64
	hostFunctions := map[string]quickjs.HostFunction{}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	bindings := make([]string, 0, len(names))
	for _, name := range names {
		hostName := interpreterHostName(name)
		tool := selected[name]
		hostFunctions[hostName] = func(callContext context.Context, arguments []any) (any, error) {
			callNumber := calls.Add(1)
			if callNumber > int64(runtime.options.MaxPTCCalls) {
				return nil, &ptcCallBudgetError{Limit: int64(runtime.options.MaxPTCCalls), Attempted: callNumber, Function: name}
			}
			payload := map[string]any{}
			if len(arguments) > 0 {
				if object, ok := arguments[0].(map[string]any); ok {
					payload = object
				}
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			childRuntime := toolRuntime
			childRuntime.CallID = fmt.Sprintf("ptc_%s_%d", name, callNumber)
			result, err := tool.Execute(callContext, raw, childRuntime)
			if err != nil {
				return nil, err
			}
			return interpreterToolValue(result), nil
		}
		bindings = append(bindings, fmt.Sprintf("%s:(input={})=>%s(input)", jsIdentifier(name), hostName))
	}
	engine, err := quickjs.New(ctx, quickjs.Options{
		MemoryLimit: runtime.options.MemoryLimit, StackLimit: runtime.options.StackLimit,
		MaxStdout: runtime.options.MaxStdoutChars, HostFunctions: hostFunctions,
	}, snapshot)
	if err != nil {
		return datool.Result{}, err
	}
	defer engine.Close(context.Background())
	code := "globalThis.tools={" + strings.Join(bindings, ",") + "};\n" + input.Code
	outcome, evalErr := engine.Eval(ctx, code, runtime.options.Timeout)
	if errors.Is(evalErr, context.Canceled) {
		return datool.Result{}, evalErr
	}
	var nextSnapshot []byte
	dirtyRecord := false
	if len(snapshot) == 0 {
		nextSnapshot, err = engine.Snapshot()
	} else {
		nextSnapshot, err = engine.DirtySnapshot()
		dirtyRecord = true
		if err == nil && len(nextSnapshot) >= len(snapshot) {
			nextSnapshot, err = engine.Snapshot()
			dirtyRecord = false
		}
	}
	if err != nil {
		if evalErr != nil {
			result := datool.TextResult(formatInterpreterError(evalErr, runtime.options.MaxResultChars))
			result.Status = damessage.ToolStatusError
			return result, nil
		}
		return datool.Result{}, err
	}
	record := encodeInterpreterSnapshot(nextSnapshot, dirtyRecord)
	if len(nextSnapshot) > runtime.options.MaxSnapshotBytes {
		record = map[string]any{"kind": "clear", "data": []byte(nil)}
	}
	if evalErr != nil {
		var hostError *quickjs.HostError
		if errors.As(evalErr, &hostError) {
			var budget *ptcCallBudgetError
			if !errors.As(hostError, &budget) {
				return datool.Result{}, hostError.Err
			}
		}
		result := datool.TextResult(formatInterpreterError(evalErr, runtime.options.MaxResultChars))
		result.Status = damessage.ToolStatusError
		result.Update = map[string]any{interpreterSnapshotKey: record}
		return result, nil
	}
	return datool.Result{
		Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: formatInterpreterOutcome(outcome, runtime.options.MaxResultChars)}},
		Update:  map[string]any{interpreterSnapshotKey: record},
	}, nil
}

func interpreterSnapshotField(maxBytes int) dagent.StateField {
	return dagent.StateField{
		Kind: dagent.FieldDelta, Contract: "dago.quickjs.snapshot.delta.v2", Private: true,
		SnapshotFrequency: 100,
		Initial:           func() any { return map[string]any{"snapshot": []byte(nil)} },
		Clone:             cloneInterpreterSnapshotValue,
		Reduce: func(current any, writes []any) (any, error) {
			base := materializedInterpreterSnapshot(current)
			if len(base) > maxBytes {
				return nil, fmt.Errorf("JavaScript snapshot exceeds %d bytes", maxBytes)
			}
			for _, write := range writes {
				record, ok := write.(map[string]any)
				if !ok {
					continue
				}
				kind, _ := record["kind"].(string)
				data, _ := record["data"].([]byte)
				switch kind {
				case "snap":
					if len(data) > maxBytes {
						return nil, fmt.Errorf("JavaScript snapshot exceeds %d bytes", maxBytes)
					}
					base = append([]byte(nil), data...)
				case "pages":
					patched, err := quickjs.ApplyDirtySnapshot(base, data, maxBytes)
					if err != nil {
						return nil, fmt.Errorf("apply JavaScript dirty pages: %w", err)
					}
					base = patched
				case "clear":
					base = nil
				}
			}
			return map[string]any{"snapshot": base}, nil
		},
	}
}

func encodeInterpreterSnapshot(snapshot []byte, dirty bool) map[string]any {
	if dirty {
		return map[string]any{"kind": "pages", "data": append([]byte(nil), snapshot...)}
	}
	return map[string]any{"kind": "snap", "data": append([]byte(nil), snapshot...)}
}

func materializedInterpreterSnapshot(value any) []byte {
	if state, ok := value.(map[string]any); ok {
		if snapshot, ok := state["snapshot"].([]byte); ok {
			return append([]byte(nil), snapshot...)
		}
	}
	return nil
}

func cloneInterpreterSnapshotValue(value any) any {
	state, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	clone := make(map[string]any, len(state))
	for key, item := range state {
		if data, ok := item.([]byte); ok {
			clone[key] = append([]byte(nil), data...)
		} else {
			clone[key] = item
		}
	}
	return clone
}

func interpreterHostName(name string) string {
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("__dago_ptc_%x", digest[:16])
}

func interpreterToolValue(result datool.Result) any {
	if len(result.Structured) > 0 {
		var value any
		if json.Unmarshal(result.Structured, &value) == nil {
			return value
		}
	}
	for index := len(result.Content) - 1; index >= 0; index-- {
		block := result.Content[index]
		if block.Type == damessage.BlockText {
			return block.Text
		}
	}
	return nil
}

func interpreterThreadKey(threadID, taskID string) string {
	if threadID != "" {
		return threadID
	}
	return taskID
}

func jsIdentifier(name string) string {
	var result strings.Builder
	upper := false
	for index, character := range name {
		if character == '_' || character == '-' {
			upper = true
			continue
		}
		if index == 0 && unicode.IsDigit(character) {
			result.WriteByte('_')
		}
		if upper {
			character = unicode.ToUpper(character)
			upper = false
		}
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '$' {
			result.WriteRune(character)
		}
	}
	value := result.String()
	if value == "" {
		return "tool"
	}
	first, _ := utf8.DecodeRuneInString(value)
	if unicode.IsDigit(first) {
		return "_" + value
	}
	return value
}

func interpreterPrompt(toolName string, tools map[string]datool.Tool) string {
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "<code_interpreter>\nUse `%s` to evaluate JavaScript in a persistent QuickJS REPL. Top-level await works. Variables and functions persist across calls and checkpoints. Use console.log for intermediate output.", toolName)
	if len(tools) > 0 {
		prompt.WriteString("\nAvailable programmatic tool calls are async and may be parallelized with Promise.all:\n")
		names := make([]string, 0, len(tools))
		for name := range tools {
			names = append(names, name)
		}
		sort.Strings(names)
		prompt.WriteString("The namespace is installed as `globalThis.tools`.\n")
		for _, name := range names {
			definition := tools[name].Definition()
			fmt.Fprintf(&prompt, "- tools.%s(input: %s): Promise<unknown> — %s\n", jsIdentifier(name), interpreterSchemaType(definition.InputSchema), definition.Description)
		}
	}
	prompt.WriteString("\n</code_interpreter>")
	return prompt.String()
}

func interpreterSchemaType(schema json.RawMessage) string {
	var document map[string]any
	if json.Unmarshal(schema, &document) != nil {
		return "Record<string, unknown>"
	}
	return interpreterJSONSchemaType(document)
}

func interpreterJSONSchemaType(schema map[string]any) string {
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		parts := make([]string, 0, len(values))
		for _, value := range values {
			raw, err := json.Marshal(value)
			if err == nil {
				parts = append(parts, string(raw))
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " | ")
		}
	}
	if alternatives, ok := schema["anyOf"].([]any); ok {
		parts := make([]string, 0, len(alternatives))
		for _, alternative := range alternatives {
			if typed, ok := alternative.(map[string]any); ok {
				parts = append(parts, interpreterJSONSchemaType(typed))
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " | ")
		}
	}
	typeName, _ := schema["type"].(string)
	switch typeName {
	case "string":
		return "string"
	case "integer", "number":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	case "array":
		if items, ok := schema["items"].(map[string]any); ok {
			itemType := interpreterJSONSchemaType(items)
			if strings.Contains(itemType, " | ") {
				itemType = "(" + itemType + ")"
			}
			return itemType + "[]"
		}
		return "unknown[]"
	case "object", "":
		properties, _ := schema["properties"].(map[string]any)
		if len(properties) == 0 {
			return "Record<string, unknown>"
		}
		required := map[string]bool{}
		if values, ok := schema["required"].([]any); ok {
			for _, value := range values {
				if name, ok := value.(string); ok {
					required[name] = true
				}
			}
		}
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			property, _ := properties[name].(map[string]any)
			optional := "?"
			if required[name] {
				optional = ""
			}
			parts = append(parts, name+optional+": "+interpreterJSONSchemaType(property))
		}
		return "{ " + strings.Join(parts, "; ") + " }"
	default:
		return "unknown"
	}
}

func formatInterpreterOutcome(outcome quickjs.Outcome, limit int) string {
	parts := []string{}
	if outcome.Stdout != "" || outcome.StdoutDropped > 0 {
		stdout := outcome.Stdout
		if outcome.StdoutDropped > 0 {
			stdout += fmt.Sprintf("… [truncated %d chars]", outcome.StdoutDropped)
		}
		parts = append(parts, "<stdout>\n"+truncateInterpreter(stdout, limit)+"\n</stdout>")
	}
	value := formatInterpreterValue(outcome.Value)
	kind := ""
	if outcome.ValueKind == "function" || outcome.ValueKind == "symbol" {
		kind = ` kind="handle"`
	}
	parts = append(parts, "<result"+kind+">"+xmlEscape(truncateInterpreter(value, limit))+"</result>")
	return strings.Join(parts, "\n")
}

func formatInterpreterError(err error, limit int) string {
	typeName := "JavaScriptError"
	if errors.Is(err, context.DeadlineExceeded) {
		typeName = "Timeout"
	} else if errors.Is(err, context.Canceled) {
		typeName = "Cancelled"
	} else {
		var budget *ptcCallBudgetError
		var deadlock *quickjs.DeadlockError
		var hostError *quickjs.HostError
		var jsError *quickjs.Error
		switch {
		case errors.As(err, &budget):
			typeName = "PTCCallBudgetExceeded"
		case errors.As(err, &hostError) && errors.As(hostError, &budget):
			typeName = "PTCCallBudgetExceeded"
		case errors.As(err, &deadlock):
			typeName = "Deadlock"
		case errors.As(err, &jsError):
			typeName = jsError.Name
		}
	}
	return `<error type="` + xmlEscape(typeName) + `">` + xmlEscape(truncateInterpreter(err.Error(), limit)) + `</error>`
}
func formatInterpreterValue(value any) string {
	if _, ok := value.(quickjs.Undefined); ok {
		return "undefined"
	}
	switch value := value.(type) {
	case nil:
		return "null"
	case string:
		return value
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(raw)
	}
}
func truncateInterpreter(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	marker := "… [truncated]"
	markerRunes := []rune(marker)
	if limit <= len(markerRunes) {
		return string(markerRunes[:limit])
	}
	return string(runes[:limit-len(markerRunes)]) + marker
}
func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(value)
}
