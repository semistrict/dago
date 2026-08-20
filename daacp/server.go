// Package daacp exposes dago agents through Agent Client Protocol version 1.
package daacp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sync"

	acp "github.com/coder/acp-go-sdk"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
)

const (
	// ConfigurableCWD is the invocation setting containing the ACP
	// session's absolute working directory.
	ConfigurableCWD = "acp.cwd"
)

// Runner is the dago execution surface required by the ACP adapter.
type Runner interface {
	Stream(context.Context, ...dagent.RunOption) *dagent.Stream
	Cancel(context.Context, ...dagent.RunOption) (dagent.Result, error)
}

// AgentSessionContext describes one ACP session before its execution target is
// built. A factory can use the selected model, working directory, and MCP
// declarations to construct an isolated runner for that client session.
type AgentSessionContext struct {
	ID                    string
	CWD                   string
	AdditionalDirectories []string
	MCPServers            []acp.McpServer
	Model                 string
}

// SessionConfig is retained as an alias for callers using the original name.
type SessionConfig = AgentSessionContext

// AgentFactory constructs a session-scoped runner. The returned closer owns
// any resources opened for that session, including MCP connections.
type AgentFactory func(context.Context, AgentSessionContext) (Runner, io.Closer, error)

// SessionFactory is retained as an alias for callers using the original name.
type SessionFactory = AgentFactory

// SessionState is the durable ACP state required by session/load. CWD is the
// original absolute working directory recorded when the thread was created.
type SessionState struct {
	Messages []damessage.Message
	CWD      string
	Model    string
}

// SessionLoader is optionally implemented by a session-scoped runner that can
// reconstruct durable ACP state for session/load without executing work.
type SessionLoader interface {
	LoadACPSession(context.Context, string) (SessionState, error)
}

// SessionConfigSaver is implemented by durable session runners that can save
// a selected model without executing a model turn.
type SessionConfigSaver interface {
	SaveACPModelSelection(context.Context, string, string) error
}

// Options configures the ACP agent identity and optional prompt content.
type Options struct {
	Name            string
	Version         string
	ImagePrompts    bool
	AudioPrompts    bool
	EmbeddedContext bool
	AuthMethods     []acp.AuthMethod
	ConfigOptions   []acp.SessionConfigOption
	LoadSession     bool
	Configurable    map[string]any
	Logger          *slog.Logger
}

// Server exposes one runner to ACP clients. Each Serve call owns an independent
// session registry and transport.
type Server struct {
	runner  Runner
	factory AgentFactory
	options Options
}

// New constructs an ACP server. It panics when runner is nil because a server
// without an execution target can never serve a prompt.
func New(runner Runner, options Options) *Server {
	if nilRunner(runner) {
		panic("ACP server: runner is required")
	}
	return &Server{runner: runner, options: compileOptions(options)}
}

// NewFactory constructs an ACP server whose required session factory is a
// positional input. The factory receives the selected model on every build.
func NewFactory(factory AgentFactory, options Options) *Server {
	if factory == nil {
		panic("ACP server: session factory is required")
	}
	return &Server{factory: factory, options: compileOptions(options)}
}

func compileOptions(options Options) Options {
	if options.Name == "" {
		options.Name = "dago"
	}
	if options.Version == "" {
		options.Version = "development"
	}
	options.Configurable = cloneConfigurable(options.Configurable)
	options.AuthMethods = append([]acp.AuthMethod(nil), options.AuthMethods...)
	options.ConfigOptions = cloneConfigOptions(options.ConfigOptions)
	return options
}

func nilRunner(runner Runner) bool {
	if runner == nil {
		return true
	}
	value := reflect.ValueOf(runner)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Serve runs a newline-delimited JSON-RPC ACP connection until the peer closes
// its input or ctx is cancelled. On cancellation, Serve closes a closable input
// stream so the transport's reader exits promptly.
func (server *Server) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	if ctx == nil {
		return fmt.Errorf("ACP server: context is required")
	}
	if input == nil || output == nil {
		return fmt.Errorf("ACP server: input and output are required")
	}

	agent := newProtocolAgent(ctx, server.runner, server.factory, server.options)
	reader := &trackingReader{Reader: input}
	writer := &trackingWriter{Writer: output}
	connection := acp.NewAgentSideConnection(agent, writer, reader)
	if server.options.Logger != nil {
		connection.SetLogger(server.options.Logger)
	}
	agent.setConnection(connection)

	select {
	case <-connection.Done():
		agent.cancelAll()
		if err := writer.Err(); err != nil {
			return fmt.Errorf("ACP server: write: %w", err)
		}
		if err := reader.Err(); err != nil {
			return fmt.Errorf("ACP server: read: %w", err)
		}
		return nil
	case <-ctx.Done():
		agent.cancelAll()
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
		return context.Cause(ctx)
	}
}

type trackingReader struct {
	io.Reader
	mu  sync.Mutex
	err error
}

func (reader *trackingReader) Read(value []byte) (int, error) {
	n, err := reader.Reader.Read(value)
	if err != nil && err != io.EOF {
		reader.mu.Lock()
		reader.err = err
		reader.mu.Unlock()
	}
	return n, err
}

func (reader *trackingReader) Err() error {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.err
}

type trackingWriter struct {
	io.Writer
	mu  sync.Mutex
	err error
}

func (writer *trackingWriter) Write(value []byte) (int, error) {
	n, err := writer.Writer.Write(value)
	if err != nil {
		writer.mu.Lock()
		writer.err = err
		writer.mu.Unlock()
	}
	return n, err
}

func (writer *trackingWriter) Err() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.err
}

func cloneConfigurable(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		switch typed := value.(type) {
		case map[string]any:
			result[key] = cloneConfigurable(typed)
		case []any:
			items := make([]any, len(typed))
			for index, item := range typed {
				items[index] = cloneConfigurable(map[string]any{"value": item})["value"]
			}
			result[key] = items
		case []string:
			result[key] = append([]string(nil), typed...)
		case map[string]string:
			items := make(map[string]string, len(typed))
			for itemKey, item := range typed {
				items[itemKey] = item
			}
			result[key] = items
		default:
			result[key] = value
		}
	}
	return result
}
