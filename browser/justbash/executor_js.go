//go:build js && wasm

package justbash

import (
	"context"
	"encoding/json"
	"fmt"
	"syscall/js"

	"github.com/semistrict/dago/browser/jsbridge"
)

// GlobalExecutor returns an Executor backed by an async JavaScript function on
// globalThis. The function receives a JSON Request and resolves to a JSON
// Response.
func GlobalExecutor(name string) Executor {
	return func(ctx context.Context, request Request) (Response, error) {
		execute := js.Global().Get(name)
		if execute.Type() != js.TypeFunction {
			return Response{}, fmt.Errorf("just-bash executor %q is unavailable", name)
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			return Response{}, fmt.Errorf("encode just-bash request: %w", err)
		}
		raw, err := jsbridge.AwaitString(ctx, execute.Invoke(string(encoded)), "just-bash execution failed")
		if err != nil {
			return Response{}, err
		}
		var response Response
		if err := json.Unmarshal([]byte(raw), &response); err != nil {
			return Response{}, fmt.Errorf("decode just-bash response: %w", err)
		}
		return response, nil
	}
}
