//go:build js && wasm

package jsbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"syscall/js"
)

// PromiseStore adapts an async global JavaScript function to the byte-oriented
// operation contract used by WASM persistence packages.
type PromiseStore struct {
	GlobalName         string
	UnavailableMessage string
	RejectionFallback  string
}

// Execute invokes the configured JavaScript function with an operation and a
// UTF-8 JSON payload.
func (store PromiseStore) Execute(ctx context.Context, operation string, payload []byte) ([]byte, error) {
	execute := js.Global().Get(store.GlobalName)
	if execute.Type() != js.TypeFunction {
		message := store.UnavailableMessage
		if message == "" {
			message = fmt.Sprintf("JavaScript store %q is unavailable", store.GlobalName)
		}
		return nil, fmt.Errorf("%s", message)
	}
	fallback := store.RejectionFallback
	if fallback == "" {
		fallback = "browser persistence operation failed"
	}
	result, err := AwaitString(ctx, execute.Invoke(operation, string(payload)), fallback)
	if err != nil {
		return nil, err
	}
	return []byte(result), nil
}

// InvokeJSON calls an async global JavaScript function using stable JSON
// request and response representations.
func InvokeJSON(ctx context.Context, name string, request, response any, fallback string) error {
	invoke := js.Global().Get(name)
	if invoke.Type() != js.TypeFunction {
		return fmt.Errorf("JavaScript function %q is unavailable", name)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", name, err)
	}
	raw, err := AwaitString(ctx, invoke.Invoke(string(encoded)), fallback)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(raw), response); err != nil {
		return fmt.Errorf("decode %s response: %w", name, err)
	}
	return nil
}

// Registry retains exported JavaScript functions for the lifetime of a WASM
// application. Release removes every registered global and releases its Go
// callback.
type Registry struct {
	functions map[string]js.Func
}

// Register exposes handler under name on globalThis and retains the callback.
func (registry *Registry) Register(name string, handler func([]js.Value) any) {
	if registry.functions == nil {
		registry.functions = make(map[string]js.Func)
	}
	if previous, exists := registry.functions[name]; exists {
		previous.Release()
	}
	function := js.FuncOf(func(_ js.Value, arguments []js.Value) any { return handler(arguments) })
	registry.functions[name] = function
	js.Global().Set(name, function)
}

// Release removes and releases all registered callbacks.
func (registry *Registry) Release() {
	for name, function := range registry.functions {
		js.Global().Delete(name)
		function.Release()
	}
	registry.functions = nil
}

// AwaitString waits for a JavaScript promise and returns its string result.
// Cancellation returns promptly while retaining callbacks until the promise
// settles, preventing syscall/js from invoking released functions.
func AwaitString(ctx context.Context, promise js.Value, fallback string) (string, error) {
	type outcome struct {
		value string
		err   error
	}
	completed := make(chan outcome, 1)
	resolve := js.FuncOf(func(_ js.Value, arguments []js.Value) any {
		value := ""
		if len(arguments) > 0 {
			value = arguments[0].String()
		}
		completed <- outcome{value: value}
		return nil
	})
	reject := js.FuncOf(func(_ js.Value, arguments []js.Value) any {
		completed <- outcome{err: fmt.Errorf("%s", RejectionMessage(arguments, fallback))}
		return nil
	})
	release := func() {
		resolve.Release()
		reject.Release()
	}
	promise.Call("then", resolve).Call("catch", reject)
	select {
	case result := <-completed:
		release()
		return result.value, result.err
	case <-ctx.Done():
		go func() {
			<-completed
			release()
		}()
		return "", ctx.Err()
	}
}

// RejectionMessage extracts a useful error message from a promise rejection.
func RejectionMessage(arguments []js.Value, fallback string) string {
	if len(arguments) == 0 {
		return fallback
	}
	value := arguments[0]
	if value.Type() == js.TypeString {
		return value.String()
	}
	if value.Type() == js.TypeObject || value.Type() == js.TypeFunction {
		message := value.Get("message")
		if message.Type() == js.TypeString && message.String() != "" {
			return message.String()
		}
	}
	stringConstructor := js.Global().Get("String")
	if stringConstructor.Type() == js.TypeFunction {
		message := stringConstructor.Invoke(value)
		if message.Type() == js.TypeString && message.String() != "" && message.String() != "[object Object]" {
			return message.String()
		}
	}
	return fallback
}

// Async returns a JavaScript promise that executes work in a goroutine.
func Async(work func() (string, error)) js.Value {
	executor := js.FuncOf(func(_ js.Value, arguments []js.Value) any {
		resolve, reject := arguments[0], arguments[1]
		go func() {
			value, err := work()
			if err != nil {
				reject.Invoke(js.Global().Get("Error").New(err.Error()))
				return
			}
			resolve.Invoke(value)
		}()
		return nil
	})
	promise := js.Global().Get("Promise").New(executor)
	executor.Release()
	return promise
}

// Resolve creates an already-resolved JavaScript promise.
func Resolve(value string) js.Value {
	return js.Global().Get("Promise").Call("resolve", value)
}

// Reject creates an already-rejected JavaScript promise.
func Reject(err error) js.Value {
	return js.Global().Get("Promise").Call("reject", js.Global().Get("Error").New(err.Error()))
}
