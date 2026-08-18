package daeventbus

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"
)

// Source forwards newline-delimited local socket events to a caller-owned Sink.
// One Source may be run repeatedly but never concurrently.
type Source struct {
	sink    Sink
	path    string
	options Options

	mu       sync.Mutex
	running  bool
	listener net.Listener
	cancel   context.CancelFunc
	clients  map[net.Conn]net.Listener
	wait     sync.WaitGroup
}

// NewUnixSource constructs a source without performing I/O. The required sink
// and absolute socket path are positional. Invalid static inputs panic.
func NewUnixSource(sink Sink, path string, options Options) *Source {
	if nilInterface(sink) {
		panic("daeventbus: sink is nil")
	}
	options = compileOptions(options)
	if path == "" || !utf8.ValidString(path) || containsControl(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path || len(path) > options.MaxSocketPathBytes {
		panic("daeventbus: socket path must be clean, absolute, and bounded")
	}
	return &Source{sink: sink, path: path, options: options, clients: map[net.Conn]net.Listener{}}
}

// Path returns the configured socket path.
func (source *Source) Path() string { return source.path }

// Run binds the socket and serves until ctx is cancelled, Close is called, or
// the listener fails. Cancellation is propagated after all compliant sinks exit.
func (source *Source) Run(ctx context.Context) error {
	if ctx == nil {
		panic("daeventbus: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	source.mu.Lock()
	if source.running {
		source.mu.Unlock()
		cancel()
		return ErrAlreadyRunning
	}
	source.running = true
	source.cancel = cancel
	source.mu.Unlock()

	listener, identity, err := listenUnix(source.path)
	if err != nil {
		cancel()
		source.finishRun(nil)
		return err
	}
	source.mu.Lock()
	source.listener = listener
	source.mu.Unlock()
	if runCtx.Err() != nil {
		source.closeActive(listener)
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-runCtx.Done()
		source.closeActive(listener)
	}()

	serveErr := source.accept(runCtx, listener)
	cancel()
	source.closeActive(listener)
	source.wait.Wait()
	<-shutdownDone
	cleanupErr := cleanupUnix(source.path, identity)
	source.finishRun(listener)

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
		return errors.Join(ErrTransport, serveErr)
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	return nil
}

// Close requests shutdown of the active Run. It is idempotent and does not
// wait for a sink that ignores its context.
func (source *Source) Close() {
	source.mu.Lock()
	cancel := source.cancel
	listener := source.listener
	source.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if listener != nil {
		source.closeActive(listener)
	}
}

func (source *Source) accept(ctx context.Context, listener net.Listener) error {
	semaphore := make(chan struct{}, source.options.MaxConcurrentConnections)
	for {
		connection, err := listener.Accept()
		if err != nil {
			return err
		}
		select {
		case semaphore <- struct{}{}:
			source.mu.Lock()
			source.clients[connection] = listener
			source.mu.Unlock()
			source.wait.Add(1)
			go func() {
				defer func() {
					connection.Close()
					source.mu.Lock()
					delete(source.clients, connection)
					source.mu.Unlock()
					<-semaphore
					source.wait.Done()
				}()
				source.handleClient(ctx, connection)
			}()
		default:
			source.writeReply(connection, Reply{OK: false, Error: "too many connections"})
			connection.Close()
		}
	}
}

func (source *Source) handleClient(ctx context.Context, connection net.Conn) {
	reader := bufio.NewReaderSize(connection, source.options.MaxLineBytes+1)
	for count := 0; ; count++ {
		_ = connection.SetReadDeadline(time.Now().Add(source.options.ClientIdleTimeout))
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(line) > source.options.MaxLineBytes {
			source.writeReply(connection, Reply{OK: false, Error: "line exceeds read limit"})
			return
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return
		}
		if len(line) == 0 {
			return
		}
		if count >= source.options.MaxEventsPerConnection {
			source.writeReply(connection, Reply{OK: false, Error: "event count exceeds limit"})
			return
		}
		event, decodeErr := decodeEvent(line, "unix:"+source.path, source.options)
		if decodeErr != nil {
			source.writeReply(connection, Reply{OK: false, Error: replyError(decodeErr), CorrelationID: recoverCorrelationID(line, source.options.MaxCorrelationBytes)})
		} else {
			sinkCtx, cancel := context.WithTimeout(ctx, source.options.SinkTimeout)
			sinkErr := callSink(sinkCtx, source.sink, event)
			cancel()
			if sinkErr != nil {
				source.writeReply(connection, Reply{OK: false, Error: "sink failed", CorrelationID: event.CorrelationID})
			} else {
				source.writeReply(connection, Reply{OK: true, CorrelationID: event.CorrelationID})
			}
		}
		if errors.Is(err, io.EOF) {
			return
		}
	}
}

func (source *Source) writeReply(connection net.Conn, reply Reply) {
	_ = connection.SetWriteDeadline(time.Now().Add(source.options.WriteTimeout))
	encoded, err := json.Marshal(reply)
	if err != nil {
		return
	}
	_, _ = connection.Write(append(encoded, '\n'))
}

func (source *Source) closeActive(listener net.Listener) {
	_ = listener.Close()
	source.mu.Lock()
	connections := make([]net.Conn, 0, len(source.clients))
	for connection, owner := range source.clients {
		if owner == listener {
			connections = append(connections, connection)
		}
	}
	source.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func (source *Source) finishRun(listener net.Listener) {
	source.mu.Lock()
	if source.listener == listener {
		source.listener = nil
	}
	source.cancel = nil
	source.running = false
	source.mu.Unlock()
}

func replyError(err error) string {
	if errors.Is(err, ErrLimitExceeded) {
		return "line exceeds read limit"
	}
	return "invalid event"
}
