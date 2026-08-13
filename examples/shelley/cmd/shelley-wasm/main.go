//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"syscall/js"

	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dawasm/browserfs"
	wasmcheckpoint "github.com/semistrict/dago/dawasm/checkpoint"
	"github.com/semistrict/dago/dawasm/jsbridge"
	"github.com/semistrict/dago/dawasm/justbash"
	"github.com/semistrict/dago/dawasm/webgpu"
	"github.com/semistrict/dago/examples/shelley/browserapp"
)

var registry jsbridge.Registry

func main() {
	saver := wasmcheckpoint.NewIndexedDBSaver(jsbridge.PromiseStore{
		GlobalName: "shelleyCheckpointStore", UnavailableMessage: "IndexedDB checkpoint store is unavailable",
	})
	filesystem, err := browserfs.New(context.Background(), jsbridge.PromiseStore{
		GlobalName: "shelleyBrowserFileStore", UnavailableMessage: "IndexedDB browser file store is unavailable",
	})
	if err != nil {
		panic(err)
	}
	app, err := browserapp.NewWithWorkspaceAndSaver(filesystem, justbash.GlobalExecutor("shelleyJustBashExecute"), saver)
	if err != nil {
		panic(err)
	}

	registry.Register("shelleyWasmRequest", func(arguments []js.Value) any {
		if len(arguments) == 0 {
			return jsbridge.Resolve(encodeError(fmt.Errorf("request argument is required")))
		}
		raw := arguments[0].String()
		return jsbridge.Async(func() (string, error) {
			var request browserapp.Request
			if err := json.Unmarshal([]byte(raw), &request); err != nil {
				return encodeError(err), nil
			}
			encoded, err := json.Marshal(app.Handle(request))
			if err != nil {
				return encodeError(err), nil
			}
			return string(encoded), nil
		})
	})
	registry.Register("shelleyWasmSnapshot", func([]js.Value) any {
		data, err := app.Snapshot()
		if err != nil {
			return encodeError(err)
		}
		return string(data)
	})
	registry.Register("shelleyWasmRestore", func(arguments []js.Value) any {
		if len(arguments) == 0 || arguments[0].String() == "" {
			return ""
		}
		if err := app.Restore([]byte(arguments[0].String())); err != nil {
			return err.Error()
		}
		return ""
	})
	registry.Register("shelleyWasmContinue", func(arguments []js.Value) any {
		if len(arguments) == 0 {
			return false
		}
		return app.Continue(arguments[0].String())
	})
	registry.Register("shelleyWasmSetEventSink", func(arguments []js.Value) any {
		if len(arguments) == 0 || arguments[0].Type() != js.TypeFunction {
			app.SetEventSink(nil)
			return nil
		}
		sink := arguments[0]
		app.SetEventSink(func(event json.RawMessage) { sink.Invoke(string(event)) })
		return nil
	})
	registry.Register("shelleyWasmConfigureWebGPUModel", func([]js.Value) any {
		model := webgpu.New(webgpu.Options{
			Profile: damodel.Profile{
				Provider: "webgpu", Model: "local-webgpu", ContextWindow: 8192,
				MaxOutputTokens: 1024, ToolCalling: true, SupportsSeparateSystemMessage: true,
			},
			InvokeGlobal: "shelleyWebGPUInvoke", InterruptGlobal: "shelleyWebGPUInterrupt",
		})
		if err := app.ConfigureWebGPUModel(model); err != nil {
			return err.Error()
		}
		return ""
	})
	registry.Register("shelleyWasmFilesystem", func(arguments []js.Value) any {
		if len(arguments) < 2 {
			return jsbridge.Reject(fmt.Errorf("filesystem operation and payload are required"))
		}
		operation, payload := arguments[0].String(), []byte(arguments[1].String())
		return jsbridge.Async(func() (string, error) {
			result, executeErr := filesystem.ExecuteJS(context.Background(), operation, payload)
			return string(result), executeErr
		})
	})
	registry.Register("shelleyWasmFilesystemPaths", func([]js.Value) any {
		encoded, _ := json.Marshal(filesystem.Paths())
		return string(encoded)
	})
	registry.Register("shelleyWasmConnectDirectory", func(arguments []js.Value) any {
		if len(arguments) == 0 {
			return jsbridge.Reject(fmt.Errorf("browser directory handle is required"))
		}
		handle := arguments[0]
		return jsbridge.Async(func() (string, error) {
			info, connectErr := filesystem.Connect(context.Background(), handle)
			if connectErr != nil {
				return "", connectErr
			}
			encoded, encodeErr := json.Marshal(info)
			return string(encoded), encodeErr
		})
	})
	registry.Register("shelleyWasmDisconnectDirectory", func([]js.Value) any {
		filesystem.Disconnect()
		return ""
	})
	ready := js.Global().Get("shelleyWasmReady")
	if ready.Type() != js.TypeFunction {
		panic("shelleyWasmReady callback is not installed")
	}
	ready.Invoke()
	select {}
}

func encodeError(err error) string {
	data, _ := json.Marshal(browserapp.Response{
		Status: 500, Headers: map[string]string{"Content-Type": "application/json"},
		Body: json.RawMessage(fmt.Sprintf(`{"error":%q}`, err.Error())),
	})
	return string(data)
}
