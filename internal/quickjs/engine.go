package quickjs

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/semistrict/dago/internal/quickjswasm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

const (
	statusOK                    = 0
	statusJSError               = 1
	hostCallError               = -1
	maxDepth                    = 128
	snapshotMagic               = "QFGS"
	snapshotSize                = 4 + 4 + 32 + 8 + 4
	deltaMagic                  = "QFGD"
	deltaHeaderSize             = 4 + 4 + 32 + 8 + 4 + 4 + 4
	dirtyPageSize               = uint32(4096)
	defaultTrackingMemoryLimit  = uint64(64 << 20)
	transformTopLevelConstToVar = 1 << 9
	transformWorkflowModule     = 1 << 10
)

var guestBuildID = sha256.Sum256(quickjswasm.TrackedGuest)

// Undefined is the distinct JavaScript undefined value.
type Undefined struct{}

// Error is an uncaught JavaScript exception.
type Error struct {
	Name, Message, Stack string
}

func (e *Error) Error() string {
	if e.Stack != "" {
		return e.Name + ": " + e.Message + "\n" + e.Stack
	}
	return e.Name + ": " + e.Message
}

// HostError reports an uncaught rejection from a Go host function. JavaScript
// can still catch the guest-visible HostError normally.
type HostError struct{ Err error }

func (e *HostError) Error() string { return e.Err.Error() }
func (e *HostError) Unwrap() error { return e.Err }

// DeadlockError reports a Promise that cannot make progress because no host
// calls remain in flight.
type DeadlockError struct{}

func (*DeadlockError) Error() string {
	return "JavaScript promise is pending with no host work to settle it"
}

// Options bounds one isolated JavaScript VM.
type Options struct {
	MemoryLimit   uint64
	StackLimit    uint64
	MaxStdout     int
	HostFunctions map[string]HostFunction
}

// HostFunction is exposed to JavaScript as an asynchronous function.
type HostFunction func(context.Context, []any) (any, error)

// Outcome is one REPL evaluation result.
type Outcome struct {
	Value         any
	Stdout        string
	StdoutDropped int
	ValueKind     string
}

// Module is one evaluated ES module namespace. Export reads and Promise
// settlement use the same Engine and must not run concurrently with Eval.
type Module struct {
	engine    *Engine
	namespace int32
}

// Engine owns one QuickJS runtime, context, and WASM linear memory.
type Engine struct {
	runtime       wazero.Runtime
	module        api.Module
	memory        api.Memory
	stack         api.MutableGlobal
	bitmapBase    api.MutableGlobal
	bitmapEnabled api.MutableGlobal
	bitmapPtr     uint32
	bitmapBytes   uint32
	deadline      atomic.Int64
	stdout        strings.Builder
	stdoutChars   int
	stdoutDropped int
	maxStdout     int
	hostFunctions map[string]HostFunction
	pending       chan hostCompletion
	pendingCount  atomic.Int64
	evalContext   context.Context
	lastHostError error
}

type hostCompletion struct {
	id    uint32
	value any
	err   error
}

// New creates an isolated VM and optionally restores a whole-memory snapshot.
func New(ctx context.Context, options Options, snapshot []byte) (*Engine, error) {
	if nilQuickJSDependency(ctx) {
		panic("QuickJS context is required")
	}
	if options.MaxStdout < 0 {
		panic("QuickJS maximum stdout cannot be negative")
	}
	if options.MemoryLimit == 0 {
		options.MemoryLimit = defaultTrackingMemoryLimit
	}
	const pageSize = uint64(65536)
	pages := (options.MemoryLimit + pageSize - 1) / pageSize
	if pages > 1<<16 {
		panic(fmt.Sprintf("QuickJS memory limit %d exceeds wasm32 capacity", options.MemoryLimit))
	}
	hostFunctions := make(map[string]HostFunction, len(options.HostFunctions))
	for name, function := range options.HostFunctions {
		if name == "" || function == nil {
			panic("QuickJS host function name and implementation are required")
		}
		hostFunctions[name] = function
	}
	e := &Engine{
		maxStdout: options.MaxStdout, hostFunctions: hostFunctions,
		pending: make(chan hostCompletion, 64),
	}
	if e.maxStdout <= 0 {
		e.maxStdout = 20_000
	}
	// The interpreter backend is portable to Go's js/wasm target; the compiler
	// backend is native-only.
	config := wazero.NewRuntimeConfigInterpreter()
	config = config.WithMemoryLimitPages(uint32(pages))
	e.runtime = wazero.NewRuntimeWithConfig(ctx, config)
	fail := func(err error) (*Engine, error) { _ = e.Close(context.Background()); return nil, err }
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, e.runtime); err != nil {
		return fail(fmt.Errorf("instantiate WASI: %w", err))
	}
	env := e.runtime.NewHostModuleBuilder("env")
	env.NewFunctionBuilder().WithFunc(e.hostCall).Export("host_call")
	env.NewFunctionBuilder().WithFunc(e.hostInterrupt).Export("host_interrupt")
	env.NewFunctionBuilder().WithFunc(e.hostModuleNormalize).Export("host_module_normalize")
	env.NewFunctionBuilder().WithFunc(e.hostModuleLoad).Export("host_module_load")
	if _, err := env.Instantiate(ctx); err != nil {
		return fail(fmt.Errorf("instantiate QuickJS host: %w", err))
	}
	compiled, err := e.runtime.CompileModule(ctx, quickjswasm.TrackedGuest)
	if err != nil {
		return fail(fmt.Errorf("compile QuickJS guest: %w", err))
	}
	module, err := e.runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("quickjs"))
	if err != nil {
		return fail(fmt.Errorf("instantiate QuickJS guest: %w", err))
	}
	e.module = module
	e.memory = module.Memory()
	stack, ok := module.ExportedGlobal("__stack_pointer").(api.MutableGlobal)
	if !ok {
		return fail(errors.New("QuickJS guest has no mutable stack pointer"))
	}
	e.stack = stack
	if err := e.setupDirtyTracking(ctx, options.MemoryLimit); err != nil {
		return fail(err)
	}
	if len(snapshot) > 0 {
		if err := e.Restore(snapshot); err != nil {
			return fail(err)
		}
		if err := e.clearDirtyPages(); err != nil {
			return fail(err)
		}
	} else {
		if options.MemoryLimit > 0 {
			if _, err := e.call(ctx, "set_memory_limit", options.MemoryLimit); err != nil {
				return fail(err)
			}
		}
		if options.StackLimit > 0 {
			if _, err := e.call(ctx, "set_max_stack_size", options.StackLimit); err != nil {
				return fail(err)
			}
		}
		if err := e.installConsole(ctx); err != nil {
			return fail(err)
		}
	}
	if err := e.installHostFunctions(ctx); err != nil {
		return fail(err)
	}
	return e, nil
}

func nilQuickJSDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (e *Engine) setupDirtyTracking(ctx context.Context, memoryLimit uint64) error {
	base, ok := e.module.ExportedGlobal("__wafl_bitmap_base").(api.MutableGlobal)
	if !ok {
		return errors.New("QuickJS guest has no mutable WAFL bitmap-base global")
	}
	enabled, ok := e.module.ExportedGlobal("__wafl_enabled").(api.MutableGlobal)
	if !ok {
		return errors.New("QuickJS guest has no mutable WAFL enabled global")
	}
	if memoryLimit == 0 {
		memoryLimit = defaultTrackingMemoryLimit
	}
	pageCount := (memoryLimit + uint64(dirtyPageSize) - 1) / uint64(dirtyPageSize)
	if pageCount == 0 || pageCount > math.MaxUint32 {
		return fmt.Errorf("QuickJS dirty-page bitmap size is invalid for memory limit %d", memoryLimit)
	}
	ptr, err := e.alloc(ctx, pageCount)
	if err != nil {
		return fmt.Errorf("allocate QuickJS dirty-page bitmap: %w", err)
	}
	e.bitmapBase, e.bitmapEnabled = base, enabled
	e.bitmapPtr, e.bitmapBytes = ptr, uint32(pageCount)
	if !e.memory.Write(ptr, make([]byte, pageCount)) {
		return errors.New("initialize QuickJS dirty-page bitmap")
	}
	base.Set(uint64(ptr))
	enabled.Set(1)
	return nil
}

func (e *Engine) installHostFunctions(ctx context.Context) error {
	if len(e.hostFunctions) == 0 {
		return nil
	}
	result, err := e.call(ctx, "global_object")
	if err != nil {
		return err
	}
	global := int32(result[0])
	defer e.freeValue(ctx, global)
	for name := range e.hostFunctions {
		data := []byte(name)
		ptr, err := e.allocWrite(ctx, data)
		if err != nil {
			return err
		}
		created, callErr := e.call(ctx, "new_function", uint64(ptr), uint64(len(data)))
		e.free(ctx, ptr, uint64(len(data)))
		if callErr != nil {
			return callErr
		}
		function := int32(created[0])
		err = e.setProp(ctx, global, name, function)
		e.freeValue(ctx, function)
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Close(ctx context.Context) error {
	if e.runtime == nil {
		return nil
	}
	err := e.runtime.Close(ctx)
	e.runtime = nil
	return err
}

func (e *Engine) installConsole(ctx context.Context) error {
	name := []byte("__dago_console")
	ptr, err := e.allocWrite(ctx, name)
	if err != nil {
		return err
	}
	defer e.free(ctx, ptr, uint64(len(name)))
	result, err := e.call(ctx, "new_function", uint64(ptr), uint64(len(name)))
	if err != nil {
		return err
	}
	fn := int32(result[0])
	globalResult, err := e.call(ctx, "global_object")
	if err != nil {
		return err
	}
	global := int32(globalResult[0])
	defer e.freeValue(ctx, global)
	defer e.freeValue(ctx, fn)
	if err := e.setProp(ctx, global, string(name), fn); err != nil {
		return err
	}
	_, err = e.evalSync(ctx, `globalThis.console={log:(...a)=>__dago_console(...a),warn:(...a)=>__dago_console(...a),error:(...a)=>__dago_console(...a)};`)
	return err
}

// Eval transforms REPL declarations, evaluates with top-level await, and drives
// the QuickJS job queue to settlement.
func (e *Engine) Eval(ctx context.Context, source string, timeout time.Duration) (Outcome, error) {
	e.stdout.Reset()
	e.stdoutChars = 0
	e.stdoutDropped = 0
	e.lastHostError = nil
	transformed, err := transform(ctx, e.runtime, "<eval>", source, transformTopLevelConstToVar)
	if err != nil {
		return Outcome{}, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	e.deadline.Store(time.Now().Add(timeout).UnixNano())
	defer e.deadline.Store(0)
	e.evalContext = ctx
	defer func() { e.evalContext = nil }()
	ptr, err := e.allocWrite(ctx, []byte(transformed))
	if err != nil {
		return Outcome{}, err
	}
	defer e.free(ctx, ptr, uint64(len(transformed)))
	out, err := e.alloc(ctx, 4)
	if err != nil {
		return Outcome{}, err
	}
	defer e.free(ctx, out, 4)
	result, err := e.call(ctx, "eval_async", uint64(ptr), uint64(len(transformed)), uint64(out))
	if err != nil {
		return Outcome{}, e.normalizeInterrupt(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	if int32(result[0]) != statusOK {
		return Outcome{}, e.normalizeInterrupt(ctx, e.statusError(ctx, int32(result[0]), "eval"))
	}
	promise, err := e.readI32(out)
	if err != nil {
		return Outcome{}, err
	}
	defer e.freeValue(ctx, promise)
	settled, rejected, err := e.awaitPromise(ctx, promise)
	if err != nil {
		return Outcome{}, err
	}
	defer e.freeValue(ctx, settled)
	if rejected {
		return Outcome{}, e.settledError(ctx, settled)
	}
	valueHandle, err := e.getProp(ctx, settled, "value")
	if err != nil {
		return Outcome{}, err
	}
	defer func() { e.freeValue(ctx, valueHandle) }()
	isPromise, err := e.isPromise(ctx, valueHandle)
	if err != nil {
		return Outcome{}, err
	}
	if isPromise {
		resolved, rejected, err := e.awaitPromise(ctx, valueHandle)
		if err != nil {
			return Outcome{}, err
		}
		e.freeValue(ctx, valueHandle)
		valueHandle = resolved
		if rejected {
			return Outcome{}, e.settledError(ctx, valueHandle)
		}
	}
	value, kind, err := e.toGo(ctx, valueHandle, 0)
	if err != nil {
		kind = e.typeName(ctx, valueHandle)
		value = "[" + kind + "]"
	}
	return Outcome{Value: value, ValueKind: kind, Stdout: e.stdout.String(), StdoutDropped: e.stdoutDropped}, nil
}

// LoadWorkflowModule parses the workflow grammar in the OXC WASM guest,
// evaluates the resulting ES module, and returns its live namespace. The
// transform preserves the script's exported meta binding and exposes the
// top-level workflow body as the __dago_workflow_result Promise export.
func (e *Engine) LoadWorkflowModule(ctx context.Context, source string, timeout time.Duration) (*Module, error) {
	e.stdout.Reset()
	e.stdoutChars = 0
	e.stdoutDropped = 0
	e.lastHostError = nil
	transformed, err := transform(ctx, e.runtime, "<workflow>", source, transformWorkflowModule)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	e.deadline.Store(time.Now().Add(timeout).UnixNano())
	defer e.deadline.Store(0)
	e.evalContext = ctx
	defer func() { e.evalContext = nil }()

	data := []byte(transformed)
	ptr, err := e.allocWrite(ctx, data)
	if err != nil {
		return nil, err
	}
	defer e.free(ctx, ptr, uint64(len(data)))
	name := []byte("<workflow>")
	namePtr, err := e.allocWrite(ctx, name)
	if err != nil {
		return nil, err
	}
	defer e.free(ctx, namePtr, uint64(len(name)))
	out, err := e.alloc(ctx, 4)
	if err != nil {
		return nil, err
	}
	defer e.free(ctx, out, 4)
	result, err := e.call(
		ctx,
		"eval_module",
		uint64(ptr), uint64(len(data)), uint64(namePtr), uint64(len(name)), uint64(out),
	)
	if err != nil {
		return nil, e.normalizeInterrupt(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int32(result[0]) != statusOK {
		return nil, e.normalizeInterrupt(ctx, e.statusError(ctx, int32(result[0]), "module eval"))
	}
	namespace, err := e.readI32(out)
	if err != nil {
		return nil, err
	}
	return &Module{engine: e, namespace: namespace}, nil
}

// Export returns the current JSON-safe value of a named module export.
func (module *Module) Export(ctx context.Context, name string) (any, string, error) {
	if module == nil || module.engine == nil {
		return nil, "", errors.New("JavaScript module is closed")
	}
	handle, err := module.engine.getProp(ctx, module.namespace, name)
	if err != nil {
		return nil, "", err
	}
	defer module.engine.freeValue(ctx, handle)
	value, kind, err := module.engine.toGo(ctx, handle, 0)
	if err != nil {
		return nil, kind, err
	}
	return value, kind, nil
}

// AwaitExport resolves a named Promise export and converts its fulfillment
// value to Go. Non-Promise exports are returned immediately.
func (module *Module) AwaitExport(ctx context.Context, name string, timeout time.Duration) (Outcome, error) {
	if module == nil || module.engine == nil {
		return Outcome{}, errors.New("JavaScript module is closed")
	}
	engine := module.engine
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	engine.deadline.Store(time.Now().Add(timeout).UnixNano())
	defer engine.deadline.Store(0)
	engine.evalContext = ctx
	defer func() { engine.evalContext = nil }()

	handle, err := engine.getProp(ctx, module.namespace, name)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { engine.freeValue(ctx, handle) }()
	isPromise, err := engine.isPromise(ctx, handle)
	if err != nil {
		return Outcome{}, err
	}
	if isPromise {
		resolved, rejected, err := engine.awaitPromise(ctx, handle)
		if err != nil {
			return Outcome{}, err
		}
		engine.freeValue(ctx, handle)
		handle = resolved
		if rejected {
			return Outcome{}, engine.settledError(ctx, handle)
		}
	}
	value, kind, err := engine.toGo(ctx, handle, 0)
	if err != nil {
		kind = engine.typeName(ctx, handle)
		value = "[" + kind + "]"
	}
	return Outcome{
		Value: value, ValueKind: kind, Stdout: engine.stdout.String(), StdoutDropped: engine.stdoutDropped,
	}, nil
}

// Close releases the module namespace handle. It does not close the Engine.
func (module *Module) Close(ctx context.Context) {
	if module == nil || module.engine == nil {
		return
	}
	module.engine.freeValue(ctx, module.namespace)
	module.engine = nil
	module.namespace = 0
}

func (e *Engine) awaitPromise(ctx context.Context, promise int32) (int32, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		if time.Now().UnixNano() >= e.deadline.Load() {
			return 0, false, context.DeadlineExceeded
		}
		if _, err := e.call(ctx, "execute_pending_jobs"); err != nil {
			return 0, false, err
		}
		stateOut, err := e.alloc(ctx, 4)
		if err != nil {
			return 0, false, err
		}
		stateCall, callErr := e.call(ctx, "promise_state", uint64(uint32(promise)), uint64(stateOut))
		state, readErr := e.readI32(stateOut)
		e.free(ctx, stateOut, 4)
		if callErr != nil {
			return 0, false, callErr
		}
		if int32(stateCall[0]) != statusOK {
			return 0, false, e.statusError(ctx, int32(stateCall[0]), "promise state")
		}
		if readErr != nil {
			return 0, false, readErr
		}
		if state == 0 {
			if e.pendingCount.Load() == 0 {
				return 0, false, &DeadlockError{}
			}
			remaining := time.Until(time.Unix(0, e.deadline.Load()))
			e.deadline.Store(0)
			select {
			case completion := <-e.pending:
				e.deadline.Store(time.Now().Add(remaining).UnixNano())
				e.pendingCount.Add(-1)
				if err := e.settle(ctx, completion); err != nil {
					return 0, false, err
				}
				continue
			case <-ctx.Done():
				return 0, false, ctx.Err()
			}
		}
		resultOut, err := e.alloc(ctx, 4)
		if err != nil {
			return 0, false, err
		}
		promiseCall, callErr := e.call(ctx, "promise_result", uint64(uint32(promise)), uint64(resultOut))
		settled, readErr := e.readI32(resultOut)
		e.free(ctx, resultOut, 4)
		if callErr != nil {
			return 0, false, callErr
		}
		if readErr != nil {
			return 0, false, readErr
		}
		if state == 2 {
			return settled, true, nil
		}
		if int32(promiseCall[0]) != statusOK {
			return 0, false, e.statusError(ctx, int32(promiseCall[0]), "promise result")
		}
		return settled, false, nil
	}
}

func (e *Engine) settledError(ctx context.Context, handle int32) error {
	guestError := e.errorFromHandle(ctx, handle)
	if interrupted := e.normalizeInterrupt(ctx, guestError); interrupted != guestError {
		return interrupted
	}
	var typed *Error
	if e.lastHostError != nil && errors.As(guestError, &typed) && typed.Name == "HostError" && typed.Message == "Host function failed" {
		return &HostError{Err: e.lastHostError}
	}
	return guestError
}

func (*Engine) normalizeInterrupt(ctx context.Context, err error) error {
	var typed *Error
	if !errors.As(err, &typed) || typed.Name != "InternalError" || !strings.Contains(strings.ToLower(typed.Message), "interrupt") {
		return err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return context.DeadlineExceeded
}

func (e *Engine) isPromise(ctx context.Context, handle int32) (bool, error) {
	out, err := e.alloc(ctx, 4)
	if err != nil {
		return false, err
	}
	defer e.free(ctx, out, 4)
	result, err := e.call(ctx, "is_promise", uint64(uint32(handle)), uint64(out))
	if err != nil {
		return false, err
	}
	if int32(result[0]) != statusOK {
		return false, e.statusError(ctx, int32(result[0]), "promise check")
	}
	value, err := e.readI32(out)
	return value != 0, err
}

func (e *Engine) evalSync(ctx context.Context, source string) (int32, error) {
	ptr, err := e.allocWrite(ctx, []byte(source))
	if err != nil {
		return 0, err
	}
	defer e.free(ctx, ptr, uint64(len(source)))
	out, err := e.alloc(ctx, 4)
	if err != nil {
		return 0, err
	}
	defer e.free(ctx, out, 4)
	r, err := e.call(ctx, "eval_code", uint64(ptr), uint64(len(source)), uint64(out))
	if err != nil {
		return 0, err
	}
	if int32(r[0]) != statusOK {
		return 0, e.statusError(ctx, int32(r[0]), "eval")
	}
	h, err := e.readI32(out)
	if err == nil {
		e.freeValue(ctx, h)
	}
	return h, err
}

func (e *Engine) hostCall(ctx context.Context, module api.Module, namePtr, nameLen, _ uint32, argc, argvPtr uint32) int32 {
	nameBytes, ok := module.Memory().Read(namePtr, nameLen)
	if !ok {
		return hostCallError
	}
	name := string(nameBytes)
	if name != "__dago_console" {
		function := e.hostFunctions[name]
		if function == nil {
			return hostCallError
		}
		arguments := make([]any, argc)
		for i := uint32(0); i < argc; i++ {
			handle, ok := module.Memory().ReadUint32Le(argvPtr + i*4)
			if !ok {
				return hostCallError
			}
			value, _, err := e.toGo(ctx, int32(handle), 0)
			if err != nil {
				return hostCallError
			}
			arguments[i] = value
		}
		out, err := e.alloc(ctx, 4)
		if err != nil {
			return hostCallError
		}
		promise, callErr := module.ExportedFunction("new_promise").Call(ctx, uint64(out))
		id, readErr := e.readI32(out)
		e.free(ctx, out, 4)
		if callErr != nil || readErr != nil || len(promise) == 0 || int32(promise[0]) == 0 {
			return hostCallError
		}
		e.pendingCount.Add(1)
		callContext := e.evalContext
		go func() {
			value, err := function(callContext, arguments)
			select {
			case e.pending <- hostCompletion{id: uint32(id), value: value, err: err}:
			case <-callContext.Done():
				e.pendingCount.Add(-1)
			}
		}()
		return int32(promise[0])
	}
	parts := make([]string, 0, argc)
	for i := uint32(0); i < argc; i++ {
		h, ok := module.Memory().ReadUint32Le(argvPtr + i*4)
		if !ok {
			return hostCallError
		}
		value, _, err := e.toGo(ctx, int32(h), 0)
		if err != nil {
			parts = append(parts, "[unprintable]")
		} else {
			parts = append(parts, formatValue(value))
		}
	}
	e.appendStdout(strings.Join(parts, " "))
	r, err := module.ExportedFunction("new_undefined").Call(ctx)
	if err != nil {
		return hostCallError
	}
	return int32(r[0])
}

func (e *Engine) settle(ctx context.Context, completion hostCompletion) error {
	if completion.err != nil {
		e.lastHostError = completion.err
		message := []byte("Host function failed")
		ptr, err := e.allocWrite(ctx, message)
		if err != nil {
			return err
		}
		defer e.free(ctx, ptr, uint64(len(message)))
		out, err := e.alloc(ctx, 4)
		if err != nil {
			return err
		}
		defer e.free(ctx, out, 4)
		result, err := e.call(ctx, "new_error", uint64(ptr), uint64(len(message)), uint64(out))
		if err != nil || int32(result[0]) != statusOK {
			return errors.New("create JavaScript host error")
		}
		handle, err := e.readI32(out)
		if err != nil {
			return err
		}
		name, err := e.newString(ctx, "HostError")
		if err != nil {
			e.freeValue(ctx, handle)
			return err
		}
		if err := e.setProp(ctx, handle, "name", name); err != nil {
			e.freeValue(ctx, name)
			e.freeValue(ctx, handle)
			return err
		}
		e.freeValue(ctx, name)
		_, err = e.call(ctx, "reject_deferred", uint64(completion.id), uint64(uint32(handle)))
		return err
	}
	handle, err := e.fromGo(ctx, completion.value, 0)
	if err != nil {
		return err
	}
	_, err = e.call(ctx, "resolve_deferred", uint64(completion.id), uint64(uint32(handle)))
	return err
}

func (e *Engine) fromGo(ctx context.Context, value any, depth int) (int32, error) {
	if depth > maxDepth {
		return 0, errors.New("host value nesting exceeds limit")
	}
	switch value := value.(type) {
	case Undefined:
		result, err := e.call(ctx, "new_undefined")
		return firstI32(result), err
	case nil:
		result, err := e.call(ctx, "new_null")
		return firstI32(result), err
	case bool:
		argument := uint64(0)
		if value {
			argument = 1
		}
		result, err := e.call(ctx, "new_bool", argument)
		return firstI32(result), err
	case int:
		return e.fromGo(ctx, int64(value), depth)
	case int64:
		if value >= -(1<<53) && value <= 1<<53 {
			result, err := e.call(ctx, "new_number", math.Float64bits(float64(value)))
			return firstI32(result), err
		}
		return e.newBigInt(ctx, strconv.FormatInt(value, 10))
	case float64:
		result, err := e.call(ctx, "new_number", math.Float64bits(value))
		return firstI32(result), err
	case string:
		return e.newString(ctx, value)
	case []any:
		result, err := e.call(ctx, "new_array")
		if err != nil {
			return 0, err
		}
		array := firstI32(result)
		for index, item := range value {
			handle, err := e.fromGo(ctx, item, depth+1)
			if err != nil {
				e.freeValue(ctx, array)
				return 0, err
			}
			set, callErr := e.call(ctx, "set_index", uint64(uint32(array)), uint64(index), uint64(uint32(handle)))
			e.freeValue(ctx, handle)
			if callErr != nil || int32(set[0]) != statusOK {
				e.freeValue(ctx, array)
				return 0, errors.New("set host array item")
			}
		}
		return array, nil
	case map[string]any:
		result, err := e.call(ctx, "new_object")
		if err != nil {
			return 0, err
		}
		object := firstI32(result)
		for key, item := range value {
			handle, err := e.fromGo(ctx, item, depth+1)
			if err != nil {
				e.freeValue(ctx, object)
				return 0, err
			}
			err = e.setProp(ctx, object, key, handle)
			e.freeValue(ctx, handle)
			if err != nil {
				e.freeValue(ctx, object)
				return 0, err
			}
		}
		return object, nil
	default:
		return e.newString(ctx, fmt.Sprint(value))
	}
}

func firstI32(values []uint64) int32 {
	if len(values) == 0 {
		return 0
	}
	return int32(values[0])
}

func (e *Engine) newString(ctx context.Context, value string) (int32, error) {
	data := []byte(value)
	ptr, err := e.allocWrite(ctx, data)
	if err != nil {
		return 0, err
	}
	defer e.free(ctx, ptr, uint64(len(data)))
	result, err := e.call(ctx, "new_string", uint64(ptr), uint64(len(data)))
	return firstI32(result), err
}

func (e *Engine) newBigInt(ctx context.Context, value string) (int32, error) {
	data := []byte(value)
	ptr, err := e.allocWrite(ctx, data)
	if err != nil {
		return 0, err
	}
	defer e.free(ctx, ptr, uint64(len(data)))
	out, err := e.alloc(ctx, 4)
	if err != nil {
		return 0, err
	}
	defer e.free(ctx, out, 4)
	result, err := e.call(ctx, "new_bigint", uint64(ptr), uint64(len(data)), uint64(out))
	if err != nil || int32(result[0]) != statusOK {
		return 0, errors.New("create JavaScript bigint")
	}
	return e.readI32(out)
}

func (e *Engine) hostInterrupt(context.Context) int32 {
	// Go's browser target is single-threaded. Yield here so cancellation and
	// timer goroutines can run while QuickJS is executing CPU-bound code.
	runtime.Gosched()
	if e.evalContext != nil {
		select {
		case <-e.evalContext.Done():
			return 1
		default:
		}
	}
	deadline := e.deadline.Load()
	if deadline != 0 && time.Now().UnixNano() >= deadline {
		return 1
	}
	return 0
}

func (*Engine) hostModuleNormalize(context.Context, api.Module, uint32, uint32, uint32, uint32, uint32) uint32 {
	return 0
}
func (*Engine) hostModuleLoad(context.Context, api.Module, uint32, uint32, uint32) uint32 { return 0 }

func (e *Engine) appendStdout(line string) {
	if e.stdoutChars > 0 {
		line = "\n" + line
	}
	lineRunes := []rune(line)
	remaining := e.maxStdout - e.stdoutChars
	if remaining <= 0 {
		e.stdoutDropped += len(lineRunes)
		return
	}
	if len(lineRunes) > remaining {
		e.stdout.WriteString(string(lineRunes[:remaining]))
		e.stdoutChars += remaining
		e.stdoutDropped += len(lineRunes) - remaining
		return
	}
	e.stdout.WriteString(line)
	e.stdoutChars += len(lineRunes)
}

// Snapshot copies the complete linear memory and mutable stack pointer using
// the quickjs-rs QFGS v1 envelope.
func (e *Engine) Snapshot() ([]byte, error) {
	if e.pendingCount.Load() != 0 {
		return nil, errors.New("cannot snapshot QuickJS while host calls are pending")
	}
	size := e.memory.Size()
	image, ok := e.memory.Read(0, size)
	if !ok {
		return nil, errors.New("read QuickJS memory")
	}
	out := make([]byte, snapshotSize+len(image))
	copy(out, snapshotMagic)
	binary.LittleEndian.PutUint32(out[4:], 1)
	copy(out[8:], guestBuildID[:])
	binary.LittleEndian.PutUint64(out[40:], uint64(size))
	binary.LittleEndian.PutUint32(out[48:], uint32(e.stack.Get()))
	copy(out[snapshotSize:], image)
	return out, nil
}

// DirtySnapshot copies only pages written since the engine was restored. The
// QFGD v1 envelope is applied to a prior QFGS image by ApplyDirtySnapshot.
func (e *Engine) DirtySnapshot() ([]byte, error) {
	if e.pendingCount.Load() != 0 {
		return nil, errors.New("cannot snapshot QuickJS while host calls are pending")
	}
	bitmap, ok := e.memory.Read(e.bitmapPtr, e.bitmapBytes)
	if !ok {
		return nil, errors.New("read QuickJS dirty-page bitmap")
	}
	memorySize := e.memory.Size()
	dirty := make([]uint32, 0)
	pageCount := (memorySize + dirtyPageSize - 1) / dirtyPageSize
	for page := uint32(0); page < pageCount; page++ {
		if page < uint32(len(bitmap)) && bitmap[page] != 0 {
			dirty = append(dirty, page)
		}
	}
	out := make([]byte, deltaHeaderSize)
	copy(out, deltaMagic)
	binary.LittleEndian.PutUint32(out[4:], 1)
	copy(out[8:], guestBuildID[:])
	binary.LittleEndian.PutUint64(out[40:], uint64(memorySize))
	binary.LittleEndian.PutUint32(out[48:], uint32(e.stack.Get()))
	binary.LittleEndian.PutUint32(out[52:], dirtyPageSize)
	binary.LittleEndian.PutUint32(out[56:], uint32(len(dirty)))
	for _, page := range dirty {
		offset := page * dirtyPageSize
		length := dirtyPageSize
		if remaining := memorySize - offset; remaining < length {
			length = remaining
		}
		contents, ok := e.memory.Read(offset, length)
		if !ok {
			return nil, fmt.Errorf("read QuickJS dirty page %d", page)
		}
		entry := make([]byte, 8+len(contents))
		binary.LittleEndian.PutUint32(entry, page)
		binary.LittleEndian.PutUint32(entry[4:], length)
		copy(entry[8:], contents)
		out = append(out, entry...)
	}
	if err := e.clearDirtyPages(); err != nil {
		return nil, err
	}
	return out, nil
}

// ApplyDirtySnapshot returns a QFGS image with a QFGD page delta applied.
func ApplyDirtySnapshot(snapshot, delta []byte, maxBytes int) ([]byte, error) {
	if len(snapshot) < snapshotSize || string(snapshot[:4]) != snapshotMagic {
		return nil, errors.New("QuickJS dirty snapshot requires a QFGS base")
	}
	baseMemorySize := binary.LittleEndian.Uint64(snapshot[40:])
	baseStack := binary.LittleEndian.Uint32(snapshot[48:])
	if binary.LittleEndian.Uint32(snapshot[4:]) != 1 || string(snapshot[8:40]) != string(guestBuildID[:]) ||
		baseMemorySize != uint64(len(snapshot)-snapshotSize) || uint64(baseStack) > baseMemorySize {
		return nil, errors.New("invalid QuickJS dirty snapshot base")
	}
	if len(delta) < deltaHeaderSize || string(delta[:4]) != deltaMagic {
		return nil, errors.New("invalid QuickJS dirty snapshot header")
	}
	if binary.LittleEndian.Uint32(delta[4:]) != 1 || string(delta[8:40]) != string(guestBuildID[:]) {
		return nil, errors.New("unsupported QuickJS dirty snapshot")
	}
	memorySize := binary.LittleEndian.Uint64(delta[40:])
	stack := binary.LittleEndian.Uint32(delta[48:])
	pageSize := binary.LittleEndian.Uint32(delta[52:])
	pageCount := binary.LittleEndian.Uint32(delta[56:])
	if maxBytes < snapshotSize || pageSize != dirtyPageSize || memorySize > uint64(maxBytes-snapshotSize) || uint64(stack) > memorySize {
		return nil, errors.New("invalid QuickJS dirty snapshot bounds")
	}
	if baseMemorySize > memorySize {
		return nil, errors.New("QuickJS dirty snapshot shrinks memory")
	}
	result := make([]byte, snapshotSize+int(memorySize))
	copy(result, snapshot)
	copy(result, snapshotMagic)
	binary.LittleEndian.PutUint64(result[40:], memorySize)
	binary.LittleEndian.PutUint32(result[48:], stack)
	offset := deltaHeaderSize
	seen := map[uint32]bool{}
	for range pageCount {
		if offset+8 > len(delta) {
			return nil, errors.New("truncated QuickJS dirty page header")
		}
		page := binary.LittleEndian.Uint32(delta[offset:])
		length := binary.LittleEndian.Uint32(delta[offset+4:])
		offset += 8
		start := uint64(page) * uint64(pageSize)
		expectedLength := uint64(pageSize)
		if start >= memorySize {
			expectedLength = 0
		} else if remaining := memorySize - start; remaining < expectedLength {
			expectedLength = remaining
		}
		if seen[page] || uint64(length) != expectedLength || offset+int(length) > len(delta) {
			return nil, errors.New("invalid QuickJS dirty page")
		}
		seen[page] = true
		copy(result[snapshotSize+int(start):], delta[offset:offset+int(length)])
		offset += int(length)
	}
	if offset != len(delta) {
		return nil, errors.New("trailing QuickJS dirty snapshot data")
	}
	return result, nil
}

func (e *Engine) clearDirtyPages() error {
	if e.bitmapBytes == 0 || !e.memory.Write(e.bitmapPtr, make([]byte, e.bitmapBytes)) {
		return errors.New("clear QuickJS dirty-page bitmap")
	}
	return nil
}

func (e *Engine) markDirtyRange(offset, length uint32) error {
	if length == 0 {
		return nil
	}
	last := uint64(offset) + uint64(length) - 1
	firstPage, lastPage := offset/dirtyPageSize, uint32(last/uint64(dirtyPageSize))
	if last > math.MaxUint32 || lastPage >= e.bitmapBytes {
		return errors.New("QuickJS host write exceeds dirty-page bitmap")
	}
	for page := firstPage; page <= lastPage; page++ {
		if !e.memory.WriteByte(e.bitmapPtr+page, 1) {
			return errors.New("mark QuickJS dirty page")
		}
	}
	return nil
}

// ValidateSnapshot validates a QFGS whole-memory image without instantiating a
// guest. maxBytes <= 0 disables the encoded-size bound.
func ValidateSnapshot(snapshot []byte, maxBytes int) error {
	if len(snapshot) < snapshotSize {
		return errors.New("QuickJS snapshot shorter than header")
	}
	if maxBytes > 0 && len(snapshot) > maxBytes {
		return fmt.Errorf("QuickJS snapshot exceeds %d bytes", maxBytes)
	}
	if string(snapshot[:4]) != snapshotMagic {
		return errors.New("invalid QuickJS snapshot magic")
	}
	if binary.LittleEndian.Uint32(snapshot[4:]) != 1 {
		return errors.New("unsupported QuickJS snapshot version")
	}
	if string(snapshot[8:40]) != string(guestBuildID[:]) {
		return errors.New("QuickJS snapshot guest build mismatch")
	}
	size := binary.LittleEndian.Uint64(snapshot[40:])
	stack := binary.LittleEndian.Uint32(snapshot[48:])
	if uint64(len(snapshot)-snapshotSize) != size || uint64(stack) > size {
		return errors.New("invalid QuickJS snapshot bounds")
	}
	return nil
}

// Restore validates and restores a QFGS v1 whole-memory snapshot.
func (e *Engine) Restore(snapshot []byte) error {
	if err := ValidateSnapshot(snapshot, 0); err != nil {
		return err
	}
	size := binary.LittleEndian.Uint64(snapshot[40:])
	stack := binary.LittleEndian.Uint32(snapshot[48:])
	if uint64(e.memory.Size()) < size {
		pages := uint32((size - uint64(e.memory.Size()) + 65535) / 65536)
		if _, ok := e.memory.Grow(pages); !ok {
			return errors.New("grow QuickJS snapshot memory")
		}
	}
	if !e.memory.Write(0, snapshot[snapshotSize:]) {
		return errors.New("restore QuickJS snapshot memory")
	}
	e.stack.Set(uint64(stack))
	return nil
}

func (e *Engine) call(ctx context.Context, name string, args ...uint64) ([]uint64, error) {
	fn := e.module.ExportedFunction(name)
	if fn == nil {
		return nil, fmt.Errorf("QuickJS export %q is missing", name)
	}
	result, err := fn.Call(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("QuickJS %s: %w", name, err)
	}
	return result, nil
}

func (e *Engine) alloc(ctx context.Context, size uint64) (uint32, error) {
	r, err := e.call(ctx, "qjs_alloc", size)
	if err != nil {
		return 0, err
	}
	ptr := uint32(r[0])
	if ptr == 0 && size != 0 {
		return 0, errors.New("QuickJS allocation failed")
	}
	return ptr, nil
}
func (e *Engine) allocWrite(ctx context.Context, data []byte) (uint32, error) {
	ptr, err := e.alloc(ctx, uint64(len(data)))
	if err != nil {
		return 0, err
	}
	if len(data) > 0 && !e.memory.Write(ptr, data) {
		e.free(ctx, ptr, uint64(len(data)))
		return 0, errors.New("write QuickJS memory")
	}
	if err := e.markDirtyRange(ptr, uint32(len(data))); err != nil {
		e.free(ctx, ptr, uint64(len(data)))
		return 0, err
	}
	return ptr, nil
}
func (e *Engine) free(ctx context.Context, ptr uint32, size uint64) {
	_, _ = e.call(ctx, "qjs_free", uint64(ptr), size)
}
func (e *Engine) freeValue(ctx context.Context, handle int32) {
	if handle != 0 {
		_, _ = e.call(ctx, "free_value", uint64(uint32(handle)))
	}
}
func (e *Engine) readI32(ptr uint32) (int32, error) {
	value, ok := e.memory.ReadUint32Le(ptr)
	if !ok {
		return 0, errors.New("read QuickJS i32")
	}
	return int32(value), nil
}

func (e *Engine) takeResult(ctx context.Context) ([]byte, error) {
	p, err := e.call(ctx, "qjs_last_ptr")
	if err != nil {
		return nil, err
	}
	n, err := e.call(ctx, "qjs_last_len")
	if err != nil {
		return nil, err
	}
	defer e.call(ctx, "qjs_result_free")
	data, ok := e.memory.Read(uint32(p[0]), uint32(n[0]))
	if !ok {
		return nil, errors.New("read QuickJS result")
	}
	return append([]byte(nil), data...), nil
}
func (e *Engine) typeName(ctx context.Context, h int32) string {
	r, err := e.call(ctx, "type_of", uint64(uint32(h)))
	if err != nil || int32(r[0]) != statusOK {
		return "unknown"
	}
	b, err := e.takeResult(ctx)
	if err != nil {
		return "unknown"
	}
	return string(b)
}
func (e *Engine) getString(ctx context.Context, h int32) (string, error) {
	r, err := e.call(ctx, "get_string", uint64(uint32(h)))
	if err != nil {
		return "", err
	}
	if int32(r[0]) != statusOK {
		return "", fmt.Errorf("get string status %d", r[0])
	}
	b, err := e.takeResult(ctx)
	return string(b), err
}
func (e *Engine) getNumber(ctx context.Context, h int32) (float64, error) {
	out, err := e.alloc(ctx, 8)
	if err != nil {
		return 0, err
	}
	defer e.free(ctx, out, 8)
	r, err := e.call(ctx, "get_number", uint64(uint32(h)), uint64(out))
	if err != nil || int32(r[0]) != statusOK {
		return 0, fmt.Errorf("get number failed")
	}
	bits, ok := e.memory.ReadUint64Le(out)
	if !ok {
		return 0, errors.New("read number")
	}
	return math.Float64frombits(bits), nil
}
func (e *Engine) getBool(ctx context.Context, h int32) (bool, error) {
	out, err := e.alloc(ctx, 4)
	if err != nil {
		return false, err
	}
	defer e.free(ctx, out, 4)
	r, err := e.call(ctx, "get_bool", uint64(uint32(h)), uint64(out))
	if err != nil || int32(r[0]) != statusOK {
		return false, fmt.Errorf("get bool failed")
	}
	v, err := e.readI32(out)
	return v != 0, err
}
func (e *Engine) getProp(ctx context.Context, obj int32, key string) (int32, error) {
	p, err := e.allocWrite(ctx, []byte(key))
	if err != nil {
		return 0, err
	}
	defer e.free(ctx, p, uint64(len(key)))
	out, err := e.alloc(ctx, 4)
	if err != nil {
		return 0, err
	}
	defer e.free(ctx, out, 4)
	r, err := e.call(ctx, "get_prop", uint64(uint32(obj)), uint64(p), uint64(len(key)), uint64(out))
	if err != nil {
		return 0, err
	}
	if int32(r[0]) != statusOK {
		return 0, e.statusError(ctx, int32(r[0]), "get property")
	}
	return e.readI32(out)
}
func (e *Engine) getIndex(ctx context.Context, obj int32, index int) (int32, error) {
	out, err := e.alloc(ctx, 4)
	if err != nil {
		return 0, err
	}
	defer e.free(ctx, out, 4)
	r, err := e.call(ctx, "get_index", uint64(uint32(obj)), uint64(index), uint64(out))
	if err != nil || int32(r[0]) != statusOK {
		return 0, fmt.Errorf("get index failed")
	}
	return e.readI32(out)
}
func (e *Engine) setProp(ctx context.Context, obj int32, key string, value int32) error {
	p, err := e.allocWrite(ctx, []byte(key))
	if err != nil {
		return err
	}
	defer e.free(ctx, p, uint64(len(key)))
	r, err := e.call(ctx, "set_prop", uint64(uint32(obj)), uint64(p), uint64(len(key)), uint64(uint32(value)))
	if err != nil {
		return err
	}
	if int32(r[0]) != statusOK {
		return fmt.Errorf("set property status %d", r[0])
	}
	return nil
}
func (e *Engine) arrayLength(ctx context.Context, h int32) (int, error) {
	v, err := e.getProp(ctx, h, "length")
	if err != nil {
		return 0, err
	}
	defer e.freeValue(ctx, v)
	n, err := e.getNumber(ctx, v)
	return int(n), err
}

func (e *Engine) toGo(ctx context.Context, h int32, depth int) (any, string, error) {
	if depth > maxDepth {
		return nil, "", errors.New("JavaScript value nesting exceeds limit")
	}
	kind := e.typeName(ctx, h)
	switch kind {
	case "undefined":
		return Undefined{}, kind, nil
	case "null":
		return nil, kind, nil
	case "boolean":
		v, err := e.getBool(ctx, h)
		return v, kind, err
	case "number":
		v, err := e.getNumber(ctx, h)
		if err == nil && v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return int64(v), kind, nil
		}
		return v, kind, err
	case "string":
		v, err := e.getString(ctx, h)
		return v, kind, err
	case "bigint":
		r, err := e.call(ctx, "get_bigint", uint64(uint32(h)))
		if err != nil || int32(r[0]) != statusOK {
			return nil, kind, errors.New("get bigint")
		}
		v, err := e.takeResult(ctx)
		return string(v) + "n", kind, err
	case "array":
		n, err := e.arrayLength(ctx, h)
		if err != nil {
			return nil, kind, err
		}
		items := make([]any, n)
		for i := range n {
			item, err := e.getIndex(ctx, h, i)
			if err != nil {
				return nil, kind, err
			}
			items[i], _, err = e.toGo(ctx, item, depth+1)
			e.freeValue(ctx, item)
			if err != nil {
				return nil, kind, err
			}
		}
		return items, kind, nil
	case "object":
		keys, err := e.ownKeys(ctx, h)
		if err != nil {
			return nil, kind, err
		}
		defer e.freeValue(ctx, keys)
		n, err := e.arrayLength(ctx, keys)
		if err != nil {
			return nil, kind, err
		}
		out := make(map[string]any, n)
		for i := range n {
			kh, err := e.getIndex(ctx, keys, i)
			if err != nil {
				return nil, kind, err
			}
			key, err := e.getString(ctx, kh)
			e.freeValue(ctx, kh)
			if err != nil {
				return nil, kind, err
			}
			vh, err := e.getProp(ctx, h, key)
			if err != nil {
				return nil, kind, err
			}
			var valueKind string
			out[key], valueKind, err = e.toGo(ctx, vh, depth+1)
			e.freeValue(ctx, vh)
			if err != nil {
				return nil, kind, err
			}
			if valueKind == "undefined" {
				delete(out, key)
			}
		}
		return out, kind, nil
	default:
		return nil, kind, fmt.Errorf("opaque JavaScript %s", kind)
	}
}
func (e *Engine) ownKeys(ctx context.Context, h int32) (int32, error) {
	out, err := e.alloc(ctx, 4)
	if err != nil {
		return 0, err
	}
	defer e.free(ctx, out, 4)
	r, err := e.call(ctx, "get_own_property_names", uint64(uint32(h)), uint64(out))
	if err != nil || int32(r[0]) != statusOK {
		return 0, errors.New("get object keys")
	}
	return e.readI32(out)
}
func (e *Engine) statusError(ctx context.Context, status int32, op string) error {
	if status == statusJSError {
		return e.takeException(ctx)
	}
	return fmt.Errorf("QuickJS %s status %d", op, status)
}
func (e *Engine) takeException(ctx context.Context) error {
	out, err := e.alloc(ctx, 4)
	if err != nil {
		return err
	}
	defer e.free(ctx, out, 4)
	r, err := e.call(ctx, "last_exception", uint64(out))
	if err != nil || int32(r[0]) != statusOK {
		return errors.New("unknown JavaScript error")
	}
	h, err := e.readI32(out)
	if err != nil {
		return err
	}
	defer e.freeValue(ctx, h)
	return e.errorFromHandle(ctx, h)
}
func (e *Engine) errorFromHandle(ctx context.Context, h int32) error {
	name := e.errorField(ctx, h, "name")
	if name == "" {
		name = "Error"
	}
	return &Error{Name: name, Message: e.errorField(ctx, h, "message"), Stack: e.errorField(ctx, h, "stack")}
}
func (e *Engine) errorField(ctx context.Context, h int32, key string) string {
	v, err := e.getProp(ctx, h, key)
	if err != nil {
		return ""
	}
	defer e.freeValue(ctx, v)
	s, _ := e.getString(ctx, v)
	return s
}

func transform(ctx context.Context, runtime wazero.Runtime, name, source string, flags uint64) (string, error) {
	compiled, err := runtime.CompileModule(ctx, quickjswasm.Transform)
	if err != nil {
		return "", fmt.Errorf("compile source transformer: %w", err)
	}
	module, err := runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("transform"))
	if err != nil {
		return "", fmt.Errorf("instantiate source transformer: %w", err)
	}
	defer module.Close(ctx)
	mem := module.Memory()
	call := func(name string, args ...uint64) ([]uint64, error) {
		return module.ExportedFunction(name).Call(ctx, args...)
	}
	alloc := func(data []byte) (uint32, error) {
		r, err := call("qjst_alloc", uint64(len(data)))
		if err != nil {
			return 0, err
		}
		p := uint32(r[0])
		if len(data) > 0 && !mem.Write(p, data) {
			return 0, errors.New("write transform memory")
		}
		return p, nil
	}
	nameBytes := []byte(name)
	np, err := alloc(nameBytes)
	if err != nil {
		return "", err
	}
	defer call("qjst_free", uint64(np), uint64(len(nameBytes)))
	data := []byte(source)
	sp, err := alloc(data)
	if err != nil {
		return "", err
	}
	defer call("qjst_free", uint64(sp), uint64(len(data)))
	defer call("qjst_result_free")
	r, err := call("qjst_transform", uint64(np), uint64(len(nameBytes)), uint64(sp), uint64(len(data)), flags)
	if err != nil {
		return "", err
	}
	status := int32(r[0])
	if status == 1 {
		return source, nil
	}
	ptr, _ := call("qjst_result_ptr")
	length, _ := call("qjst_result_len")
	if status != 0 {
		ptr, _ = call("qjst_error_ptr")
		length, _ = call("qjst_error_len")
	}
	b, ok := mem.Read(uint32(ptr[0]), uint32(length[0]))
	if !ok {
		return "", errors.New("read transform result")
	}
	if status != 0 {
		return "", &Error{Name: "SyntaxError", Message: string(b)}
	}
	return string(b), nil
}

func formatValue(value any) string {
	switch v := value.(type) {
	case Undefined:
		return "undefined"
	case nil:
		return "null"
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(v)
	}
}
