//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"syscall/js"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/examples/shelley/browserapp"
)

var retained []js.Func

func main() {
	saver := browserapp.NewIndexedDBSaver(jsCheckpointStore{})
	app, err := browserapp.NewWithShellAndSaver(executeJustBash, saver)
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
	register("shelleyWasmConfigureWebGPUModel", func([]js.Value) any {
		if err := app.ConfigureWebGPUModel(webGPUChat{}); err != nil {
			return err.Error()
		}
		return ""
	})
	register("shelleyWasmReplaceWorkspace", func(arguments []js.Value) any {
		if len(arguments) == 0 {
			return "workspace argument is required"
		}
		var files map[string]dabackend.FileData
		if err := json.Unmarshal([]byte(arguments[0].String()), &files); err != nil {
			return fmt.Sprintf("decode browser workspace: %v", err)
		}
		if err := app.ReplaceWorkspace(files); err != nil {
			return err.Error()
		}
		return ""
	})
	register("shelleyWasmWorkspaceSnapshot", func([]js.Value) any {
		encoded, err := json.Marshal(app.WorkspaceSnapshot())
		if err != nil {
			return encodeError(err)
		}
		return string(encoded)
	})

	ready := js.Global().Get("shelleyWasmReady")
	if ready.Type() != js.TypeFunction {
		panic("shelleyWasmReady callback is not installed")
	}
	ready.Invoke()
	select {}
}

type jsCheckpointStore struct{}

func (jsCheckpointStore) Execute(ctx context.Context, operation string, payload []byte) ([]byte, error) {
	execute := js.Global().Get("shelleyCheckpointStore")
	if execute.Type() != js.TypeFunction {
		return nil, fmt.Errorf("IndexedDB checkpoint store is unavailable")
	}
	result, err := awaitContextPromiseString(ctx, execute.Invoke(operation, string(payload)))
	if err != nil {
		return nil, err
	}
	return []byte(result), nil
}

type webGPUChat struct{}

func (webGPUChat) Profile() damodel.Profile {
	return damodel.Profile{
		Provider: "webgpu", Model: "local-webgpu", ContextWindow: 8192,
		MaxOutputTokens: 1024, ToolCalling: true, SupportsSeparateSystemMessage: true,
	}
}

func (webGPUChat) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	invoke := js.Global().Get("shelleyWebGPUInvoke")
	if invoke.Type() != js.TypeFunction {
		return damodel.Response{}, fmt.Errorf("WebGPU model is unavailable")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return damodel.Response{}, fmt.Errorf("encode WebGPU request: %w", err)
	}
	raw, err := awaitWebGPU(ctx, invoke.Invoke(string(encoded)))
	if err != nil {
		return damodel.Response{}, err
	}
	var response damodel.Response
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return damodel.Response{}, fmt.Errorf("decode WebGPU response: %w", err)
	}
	return response, nil
}

func (webGPUChat) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return damodel.EmptyStream{}, nil
}

func awaitWebGPU(ctx context.Context, promise js.Value) (string, error) {
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
		message := jsRejectionMessage(arguments, "WebGPU inference failed")
		completed <- outcome{err: fmt.Errorf("%s", message)}
		return nil
	})
	promise.Call("then", resolve).Call("catch", reject)
	select {
	case <-ctx.Done():
		interrupt := js.Global().Get("shelleyWebGPUInterrupt")
		if interrupt.Type() == js.TypeFunction {
			interrupt.Invoke()
		}
	case result := <-completed:
		resolve.Release()
		reject.Release()
		return result.value, result.err
	}
	result := <-completed
	resolve.Release()
	reject.Release()
	if result.err != nil {
		return "", result.err
	}
	return "", ctx.Err()
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
		message := jsRejectionMessage(arguments, "just-bash execution failed")
		completed <- outcome{err: fmt.Errorf("%s", message)}
		return nil
	})
	promise.Call("then", resolve).Call("catch", reject)
	result := <-completed
	resolve.Release()
	reject.Release()
	return result.value, result.err
}

func awaitContextPromiseString(ctx context.Context, promise js.Value) (string, error) {
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
		message := jsRejectionMessage(arguments, "IndexedDB operation failed")
		completed <- outcome{err: fmt.Errorf("%s", message)}
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

func jsRejectionMessage(arguments []js.Value, fallback string) string {
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
