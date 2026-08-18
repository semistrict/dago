//go:build js && wasm

package webgpu

import (
	"context"
	"encoding/json"
	"fmt"
	"syscall/js"

	"github.com/semistrict/dago/browser/jsbridge"
	"github.com/semistrict/dago/damodel"
)

// Chat implements damodel.Chat through an async JavaScript inference function.
type Chat struct {
	options Options
}

// New constructs a browser WebGPU chat adapter.
func New(options Options) *Chat {
	return &Chat{options: compileOptions(options)}
}

func (chat *Chat) Profile() damodel.Profile { return cloneProfile(chat.options.Profile) }

func (chat *Chat) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	invoke := js.Global().Get(chat.options.InvokeGlobal)
	if invoke.Type() != js.TypeFunction {
		return damodel.Response{}, fmt.Errorf("WebGPU model bridge %q is unavailable", chat.options.InvokeGlobal)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return damodel.Response{}, fmt.Errorf("encode WebGPU request: %w", err)
	}
	raw, err := chat.await(ctx, invoke.Invoke(string(encoded)))
	if err != nil {
		return damodel.Response{}, err
	}
	var response damodel.Response
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return damodel.Response{}, fmt.Errorf("decode WebGPU response: %w", err)
	}
	return response, nil
}

func (chat *Chat) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return damodel.EmptyStream{}, nil
}

func (chat *Chat) await(ctx context.Context, promise js.Value) (string, error) {
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
		message := jsbridge.RejectionMessage(arguments, "WebGPU inference failed")
		completed <- outcome{err: fmt.Errorf("%s", message)}
		return nil
	})
	promise.Call("then", resolve).Call("catch", reject)
	select {
	case <-ctx.Done():
		interrupt := js.Global().Get(chat.options.InterruptGlobal)
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
