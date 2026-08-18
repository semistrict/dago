package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/creack/pty"
	"github.com/semistrict/dago/internal/dacode/xtermjs"
)

const defaultXtermJSAddress = "127.0.0.1:0"

type xtermJSServerOptions struct {
	Address   string
	Arguments []string
	Stdout    io.Writer
	Stderr    io.Writer
}

type xtermClientMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

func serveXtermJS(ctx context.Context, options xtermJSServerOptions) error {
	if err := validateXtermJSAddress(options.Address); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", options.Address)
	if err != nil {
		return fmt.Errorf("listen for xterm.js: %w", err)
	}
	defer listener.Close()

	mux := http.NewServeMux()
	mux.Handle("GET /", xtermjs.Handler())
	mux.HandleFunc("GET /terminal", func(response http.ResponseWriter, request *http.Request) {
		serveXtermSession(response, request, options.Arguments, options.Stderr)
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	host := listener.Addr().String()
	if strings.HasPrefix(host, "[::1]:") {
		host = "localhost:" + strings.TrimPrefix(host, "[::1]:")
	}
	fmt.Fprintf(options.Stdout, "dacode xterm.js: http://%s\n", host)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

func validateXtermJSAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("xterm.js address %q: %w", address, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("xterm.js address must use a loopback host")
	}
	return nil
}

func serveXtermSession(response http.ResponseWriter, request *http.Request, arguments []string, stderr io.Writer) {
	connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	var initial xtermClientMessage
	if err := wsjson.Read(ctx, connection, &initial); err != nil || initial.Type != "init" {
		_ = connection.Close(websocket.StatusPolicyViolation, "expected terminal init")
		return
	}
	initial.Cols, initial.Rows = terminalSize(initial.Cols, initial.Rows)

	executable, err := os.Executable()
	if err != nil {
		writeXtermError(ctx, connection, err)
		return
	}
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Env = xtermSessionEnvironment(os.Environ())
	master, err := pty.StartWithSize(command, &pty.Winsize{Cols: initial.Cols, Rows: initial.Rows})
	if err != nil {
		writeXtermError(ctx, connection, err)
		return
	}
	defer master.Close()

	runDone := make(chan error, 1)
	go func() { runDone <- command.Wait() }()
	outputDone := make(chan error, 1)
	go func() { outputDone <- pumpXtermOutput(ctx, connection, master) }()
	inputDone := make(chan error, 1)
	go func() { inputDone <- pumpXtermInput(ctx, connection, master) }()

	var runErr error
	select {
	case runErr = <-runDone:
		select {
		case <-outputDone:
		case <-time.After(time.Second):
		}
	case <-inputDone:
		cancel()
		_ = master.Close()
		select {
		case runErr = <-runDone:
		case <-time.After(2 * time.Second):
		}
	case <-request.Context().Done():
		cancel()
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) && stderr != nil {
		fmt.Fprintf(stderr, "xterm.js session: %v\n", runErr)
	}
	_ = connection.Close(websocket.StatusNormalClosure, "terminal session ended")
}

func terminalSize(cols, rows uint16) (uint16, uint16) {
	if cols < 20 {
		cols = 80
	}
	if rows < 10 {
		rows = 24
	}
	return min(cols, 500), min(rows, 200)
}

func pumpXtermOutput(ctx context.Context, connection *websocket.Conn, master *os.File) error {
	buffer := make([]byte, 32*1024)
	for {
		count, err := master.Read(buffer)
		if count > 0 {
			if writeErr := connection.Write(ctx, websocket.MessageBinary, buffer[:count]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			return err
		}
	}
}

func pumpXtermInput(ctx context.Context, connection *websocket.Conn, master *os.File) error {
	for {
		var message xtermClientMessage
		if err := wsjson.Read(ctx, connection, &message); err != nil {
			return err
		}
		switch message.Type {
		case "input":
			if _, err := io.WriteString(master, message.Data); err != nil {
				return err
			}
		case "resize":
			cols, rows := terminalSize(message.Cols, message.Rows)
			if err := pty.Setsize(master, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
				return err
			}
		case "redraw":
			cols, rows := terminalSize(message.Cols, message.Rows)
			probeCols := cols - 1
			if probeCols < 20 {
				probeCols = cols + 1
			}
			if err := pty.Setsize(master, &pty.Winsize{Cols: probeCols, Rows: rows}); err != nil {
				return err
			}
			if err := pty.Setsize(master, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
				return err
			}
		}
	}
}

func writeXtermError(ctx context.Context, connection *websocket.Conn, err error) {
	payload, _ := json.Marshal(xtermClientMessage{Type: "error", Data: err.Error()})
	_ = connection.Write(ctx, websocket.MessageText, payload)
	_ = connection.Close(websocket.StatusInternalError, "terminal startup failed")
}

func xtermSessionArguments(arguments []string) []string {
	result := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--serve-xtermjs":
			continue
		case argument == "--xtermjs-address":
			index++
			continue
		case strings.HasPrefix(argument, "--xtermjs-address="):
			continue
		default:
			result = append(result, argument)
		}
	}
	return result
}

func xtermSessionEnvironment(environment []string) []string {
	overrides := map[string]string{
		"TERM":           "xterm-256color",
		"COLORTERM":      "truecolor",
		"CLICOLOR":       "1",
		"CLICOLOR_FORCE": "1",
		"DACODE_XTERMJS": "1",
	}
	result := make([]string, 0, len(environment)+len(overrides))
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if !found || name == "NO_COLOR" {
			continue
		}
		if _, overridden := overrides[name]; overridden {
			continue
		}
		result = append(result, entry)
	}
	for _, name := range []string{"TERM", "COLORTERM", "CLICOLOR", "CLICOLOR_FORCE", "DACODE_XTERMJS"} {
		result = append(result, name+"="+overrides[name])
	}
	return result
}
