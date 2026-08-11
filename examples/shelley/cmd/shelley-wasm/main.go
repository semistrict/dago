//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"syscall/js"

	"github.com/semistrict/dago/examples/shelley/browserapp"
)

var retained []js.Func

func main() {
	app, err := browserapp.NewWithShell(executeJustBash)
	if err != nil {
		panic(err)
	}

	register("shelleyWasmRequest", func(arguments []js.Value) any {
		if len(arguments) == 0 {
			return resolvedPromise(encodeError(fmt.Errorf("request argument is required")))
		}
		raw := arguments[0].String()
		return asyncString(func() string {
			var request browserapp.Request
			if err := json.Unmarshal([]byte(raw), &request); err != nil {
				return encodeError(err)
			}
			encoded, err := json.Marshal(app.Handle(request))
			if err != nil {
				return encodeError(err)
			}
			return string(encoded)
		})
	})
	register("shelleyWasmSnapshot", func([]js.Value) any {
		data, err := app.Snapshot()
		if err != nil {
			return encodeError(err)
		}
		return string(data)
	})
	register("shelleyWasmRestore", func(arguments []js.Value) any {
		if len(arguments) == 0 || arguments[0].String() == "" {
			return ""
		}
		if err := app.Restore([]byte(arguments[0].String())); err != nil {
			return err.Error()
		}
		return ""
	})
	register("shelleyWasmContinue", func(arguments []js.Value) any {
		if len(arguments) == 0 {
			return false
		}
		return app.Continue(arguments[0].String())
	})
	register("shelleyWasmSetEventSink", func(arguments []js.Value) any {
		if len(arguments) == 0 || arguments[0].Type() != js.TypeFunction {
			app.SetEventSink(nil)
			return nil
		}
		sink := arguments[0]
		app.SetEventSink(func(event json.RawMessage) { sink.Invoke(string(event)) })
		return nil
	})

	ready := js.Global().Get("shelleyWasmReady")
	if ready.Type() != js.TypeFunction {
		panic("shelleyWasmReady callback is not installed")
	}
	ready.Invoke()
	select {}
}

func executeJustBash(_ context.Context, request browserapp.ShellRequest) (browserapp.ShellResponse, error) {
	execute := js.Global().Get("shelleyJustBashExecute")
	if execute.Type() != js.TypeFunction {
		return browserapp.ShellResponse{}, fmt.Errorf("just-bash executor is unavailable")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return browserapp.ShellResponse{}, fmt.Errorf("encode just-bash request: %w", err)
	}
	raw, err := awaitPromiseString(execute.Invoke(string(encoded)))
	if err != nil {
		return browserapp.ShellResponse{}, err
	}
	var response browserapp.ShellResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return browserapp.ShellResponse{}, fmt.Errorf("decode just-bash response: %w", err)
	}
	return response, nil
}

func awaitPromiseString(promise js.Value) (string, error) {
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
		message := "just-bash execution failed"
		if len(arguments) > 0 {
			message = arguments[0].String()
		}
		completed <- outcome{err: fmt.Errorf("%s", message)}
		return nil
	})
	promise.Call("then", resolve).Call("catch", reject)
	result := <-completed
	resolve.Release()
	reject.Release()
	return result.value, result.err
}

func asyncString(work func() string) js.Value {
	executor := js.FuncOf(func(_ js.Value, arguments []js.Value) any {
		resolve := arguments[0]
		go func() { resolve.Invoke(work()) }()
		return nil
	})
	promise := js.Global().Get("Promise").New(executor)
	executor.Release()
	return promise
}

func resolvedPromise(value string) js.Value {
	return js.Global().Get("Promise").Call("resolve", value)
}

func register(name string, handler func([]js.Value) any) {
	function := js.FuncOf(func(_ js.Value, arguments []js.Value) any { return handler(arguments) })
	retained = append(retained, function)
	js.Global().Set(name, function)
}

func encodeError(err error) string {
	data, _ := json.Marshal(browserapp.Response{
		Status: 500, Headers: map[string]string{"Content-Type": "application/json"},
		Body: json.RawMessage(fmt.Sprintf(`{"error":%q}`, err.Error())),
	})
	return string(data)
}
