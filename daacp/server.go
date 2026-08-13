// Package daacp exposes dago agents through Agent Client Protocol version 1.
package daacp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	acp "github.com/coder/acp-go-sdk"
	"github.com/semistrict/dago/dagent"
)

const (
	// ConfigurableCWD is the dagent.Input.Configurable key containing the ACP
	// session's absolute working directory.
	ConfigurableCWD = "acp.cwd"
)

// Runner is the dago execution surface required by the ACP adapter.
type Runner interface {
	Stream(context.Context, dagent.Input) *dagent.Stream
	Cancel(context.Context, dagent.Input) (dagent.Result, error)
}

// Options configures the ACP agent identity and optional prompt content.
type Options struct {
	Name            string
	Version         string
	ImagePrompts    bool
	AudioPrompts    bool
	EmbeddedContext bool
	Configurable    map[string]any
	Logger          *slog.Logger
}

// Server exposes one runner to ACP clients. Each Serve call owns an independent
// session registry and transport.
type Server struct {
	runner  Runner
	options Options
}

// New constructs an ACP server. It panics when runner is nil because a server
// without an execution target can never serve a prompt.
func New(runner Runner, options Options) *Server {
	if runner == nil {
		panic("ACP server: runner is required")
	}
	if options.Name == "" {
		options.Name = "dago"
	}
	if options.Version == "" {
		options.Version = "development"
	}
	options.Configurable = cloneConfigurable(options.Configurable)
	return &Server{runner: runner, options: options}
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

	agent := newProtocolAgent(ctx, server.runner, server.options)
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
