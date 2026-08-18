package dacode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	defaultLocalDevEndpoint        = "http://127.0.0.1:2024"
	defaultLocalDevHealthPath      = "/ok"
	defaultLocalDevStartupTimeout  = 30 * time.Second
	defaultLocalDevShutdownTimeout = 3 * time.Second
	defaultLocalDevKillTimeout     = 2 * time.Second
	defaultLocalDevProbeTimeout    = 2 * time.Second
	defaultLocalDevPollInterval    = 100 * time.Millisecond
	defaultLocalDevLogBytes        = 16 << 10
	maxLocalDevLogBytes            = 64 << 10
	maxLocalDevEnvironment         = 64
	maxLocalDevEnvironmentValue    = 64 << 10
	maxLocalDevArguments           = 128
	maxLocalDevArgumentBytes       = 16 << 10
)

var (
	errLocalDevAlreadyStarted = errors.New("local development server is already started")
	errLocalDevClosed         = errors.New("local development server is closed")

	localDevSecretAssignment = regexp.MustCompile(`(?i)\b(authorization|api[_-]?key|cookie|credential|license[_-]?key|passw(?:or)?d|secret|token)(\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	localDevBearerSecret     = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)
	localDevURLUserinfo      = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)
	localDevKnownSecret      = regexp.MustCompile(`(?i)\b(?:sk-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9_]{8,}|xox[baprs]-[A-Za-z0-9-]{8,})`)
)

// localDevServerConfig contains only non-secret child configuration. The
// subprocess receives no ambient environment except keys explicitly named in
// InheritEnvironment and a small platform baseline needed to execute programs.
type localDevServerConfig struct {
	Endpoint           string
	HealthPath         string
	Directory          string
	Environment        map[string]string
	InheritEnvironment []string
	StartupTimeout     time.Duration
	ShutdownTimeout    time.Duration
	KillTimeout        time.Duration
	ProbeTimeout       time.Duration
	PollInterval       time.Duration
	LogBytes           int

	lookupEnvironment func(string) (string, bool)
	processFactory    localDevProcessFactory
	probe             localDevReadinessProbe
	sleep             localDevSleep
}

type localDevProcess interface {
	Done() <-chan struct{}
	WaitError() error
	SignalTree() error
	KillTree() error
}

type localDevProcessLaunch struct {
	Directory   string
	Environment []string
	Output      io.Writer
}

type localDevProcessFactory func(string, []string, localDevProcessLaunch) (localDevProcess, error)
type localDevReadinessProbe func(context.Context, *url.URL) (bool, error)
type localDevSleep func(context.Context, time.Duration) error

type localDevServer struct {
	executable string
	arguments  []string
	config     localDevServerConfig
	endpoint   *url.URL
	log        *localDevLog

	lifecycle sync.Mutex
	stateMu   sync.Mutex
	process   localDevProcess
	starting  context.CancelFunc
	closed    bool
	closeErr  error
	closeDone chan struct{}
}

// newLocalDevServer validates and compiles static launch configuration without
// resolving the executable, reading the environment, or performing network I/O.
// Required executable and argv dependencies are positional; invalid static
// input is a programmer error and panics.
func newLocalDevServer(executable string, arguments []string, config localDevServerConfig) *localDevServer {
	executable = strings.TrimSpace(executable)
	if executable == "" || strings.IndexByte(executable, 0) >= 0 || !filepath.IsAbs(executable) {
		panic("dacode: local development server executable must be an absolute path")
	}
	if len(arguments) > maxLocalDevArguments {
		panic("dacode: too many local development server arguments")
	}
	arguments = append([]string(nil), arguments...)
	for _, argument := range arguments {
		if strings.IndexByte(argument, 0) >= 0 || len(argument) > maxLocalDevArgumentBytes {
			panic("dacode: invalid local development server argument")
		}
	}
	config = normalizeLocalDevServerConfig(config)
	endpoint, err := validateLocalDevEndpoint(config.Endpoint, config.HealthPath)
	if err != nil {
		panic("dacode: " + err.Error())
	}
	return &localDevServer{
		executable: executable, arguments: arguments, config: config,
		endpoint: endpoint, log: newLocalDevLog(config.LogBytes), closeDone: make(chan struct{}),
	}
}

func normalizeLocalDevServerConfig(config localDevServerConfig) localDevServerConfig {
	if config.Endpoint == "" {
		config.Endpoint = defaultLocalDevEndpoint
	}
	if config.HealthPath == "" {
		config.HealthPath = defaultLocalDevHealthPath
	}
	applyLocalDevDurationDefault(&config.StartupTimeout, defaultLocalDevStartupTimeout, 2*time.Minute, "startup timeout")
	applyLocalDevDurationDefault(&config.ShutdownTimeout, defaultLocalDevShutdownTimeout, 30*time.Second, "shutdown timeout")
	applyLocalDevDurationDefault(&config.KillTimeout, defaultLocalDevKillTimeout, 10*time.Second, "kill timeout")
	applyLocalDevDurationDefault(&config.ProbeTimeout, defaultLocalDevProbeTimeout, 10*time.Second, "probe timeout")
	applyLocalDevDurationDefault(&config.PollInterval, defaultLocalDevPollInterval, 5*time.Second, "poll interval")
	if config.LogBytes == 0 {
		config.LogBytes = defaultLocalDevLogBytes
	}
	if config.LogBytes < 1 || config.LogBytes > maxLocalDevLogBytes {
		panic("dacode: local development server log bound is out of range")
	}
	config.Environment = cloneAndValidateLocalDevEnvironment(config.Environment)
	config.InheritEnvironment = normalizeLocalDevEnvironmentAllowlist(config.InheritEnvironment)
	if config.lookupEnvironment == nil {
		config.lookupEnvironment = os.LookupEnv
	}
	if config.processFactory == nil {
		config.processFactory = startLocalDevExecProcess
	}
	if config.probe == nil {
		config.probe = probeLocalDevEndpoint
	}
	if config.sleep == nil {
		config.sleep = sleepLocalDevServer
	}
	return config
}

func applyLocalDevDurationDefault(value *time.Duration, fallback, maximum time.Duration, name string) {
	if *value == 0 {
		*value = fallback
	}
	if *value < 0 || *value > maximum {
		panic("dacode: local development server " + name + " is out of range")
	}
}

func validateLocalDevEndpoint(endpoint, healthPath string) (*url.URL, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("local development server endpoint must be a credential-free HTTP origin")
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "localhost" {
		hostname = "127.0.0.1"
	}
	ip := net.ParseIP(hostname)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("local development server endpoint must use a loopback IP")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("local development server endpoint must include a valid port")
	}
	if !strings.HasPrefix(healthPath, "/") || strings.HasPrefix(healthPath, "//") {
		return nil, errors.New("local development server health path must be absolute")
	}
	health, err := url.Parse(healthPath)
	if err != nil || health.IsAbs() || health.Host != "" || health.RawQuery != "" || health.ForceQuery || health.Fragment != "" {
		return nil, errors.New("local development server health path is invalid")
	}
	parsed.Scheme = "http"
	parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
	parsed.Path, parsed.RawPath = health.Path, ""
	parsed.Host = net.JoinHostPort(hostname, strconv.Itoa(port))
	return parsed, nil
}

func cloneAndValidateLocalDevEnvironment(environment map[string]string) map[string]string {
	if len(environment) > maxLocalDevEnvironment {
		panic("dacode: too many local development server environment values")
	}
	result := make(map[string]string, len(environment))
	for key, value := range environment {
		validateLocalDevEnvironmentKey(key)
		if len(value) > maxLocalDevEnvironmentValue || strings.IndexByte(value, 0) >= 0 {
			panic("dacode: invalid local development server environment value")
		}
		result[key] = value
	}
	return result
}

func normalizeLocalDevEnvironmentAllowlist(keys []string) []string {
	keys = append(defaultLocalDevEnvironmentAllowlist(), keys...)
	if len(keys) > maxLocalDevEnvironment {
		panic("dacode: local development server environment allowlist is too large")
	}
	seen := make(map[string]struct{}, len(keys))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		validateLocalDevEnvironmentKey(key)
		folded := strings.ToUpper(key)
		if _, exists := seen[folded]; exists {
			continue
		}
		seen[folded] = struct{}{}
		result = append(result, key)
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToUpper(result[i]) < strings.ToUpper(result[j]) })
	return result
}

func validateLocalDevEnvironmentKey(key string) {
	if key == "" || len(key) > 128 || strings.IndexByte(key, '=') >= 0 || strings.IndexByte(key, 0) >= 0 {
		panic("dacode: invalid local development server environment key")
	}
	for index, character := range key {
		if !(character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || index > 0 && character >= '0' && character <= '9') {
			panic("dacode: invalid local development server environment key")
		}
	}
	upper := strings.ToUpper(key)
	for _, marker := range []string{"AUTH", "COOKIE", "CREDENTIAL", "LICENSE", "PASSWORD", "PASSWD", "SECRET", "TOKEN", "APIKEY", "API_KEY"} {
		if strings.Contains(upper, marker) {
			panic("dacode: credentials cannot be passed to the local development server environment")
		}
	}
	for _, denied := range []string{"DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "GIT_ASKPASS", "LD_AUDIT", "LD_LIBRARY_PATH", "LD_PRELOAD", "NODE_OPTIONS", "PYTHONEXECUTABLE", "PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP", "SSH_ASKPASS"} {
		if upper == denied {
			panic("dacode: unsafe local development server environment key")
		}
	}
}

func defaultLocalDevEnvironmentAllowlist() []string {
	result := []string{"HOME", "LANG", "LC_ALL", "PATH", "TMPDIR"}
	if runtime.GOOS == "windows" {
		result = []string{"COMSPEC", "PATH", "SYSTEMROOT", "TEMP", "TMP", "USERPROFILE", "WINDIR"}
	}
	return result
}

func (server *localDevServer) Endpoint() string {
	if server == nil || server.endpoint == nil {
		panic("dacode: initialized local development server is required")
	}
	endpoint := *server.endpoint
	endpoint.Path = ""
	return strings.TrimSuffix(endpoint.String(), "/")
}

func (server *localDevServer) Start(ctx context.Context) error {
	if server == nil || server.endpoint == nil {
		panic("dacode: initialized local development server is required")
	}
	server.lifecycle.Lock()
	defer server.lifecycle.Unlock()
	return server.startLocked(ctx)
}

func (server *localDevServer) startLocked(ctx context.Context) error {
	server.stateMu.Lock()
	if server.closed {
		server.stateMu.Unlock()
		return errLocalDevClosed
	}
	if server.process != nil {
		server.stateMu.Unlock()
		return errLocalDevAlreadyStarted
	}
	startupContext, cancel := context.WithTimeout(ctx, server.config.StartupTimeout)
	server.starting = cancel
	server.stateMu.Unlock()
	defer func() {
		cancel()
		server.stateMu.Lock()
		server.starting = nil
		server.stateMu.Unlock()
	}()

	launch := localDevProcessLaunch{
		Directory: server.config.Directory, Environment: server.childEnvironment(), Output: server.log,
	}
	server.log.Reset()
	process, err := server.config.processFactory(server.executable, append([]string(nil), server.arguments...), launch)
	if err != nil {
		return fmt.Errorf("start local development server: %w", errors.New(sanitizeLocalDevDiagnostic(err.Error(), server.config.LogBytes)))
	}
	if process == nil {
		return errors.New("start local development server: process factory returned no process")
	}
	server.stateMu.Lock()
	server.process = process
	closed := server.closed
	server.stateMu.Unlock()
	if closed {
		err := server.stopProcess(context.Background(), process)
		server.clearProcess(process)
		return errors.Join(errLocalDevClosed, err)
	}

	if err := server.waitReady(startupContext, process); err != nil {
		stopErr := server.stopProcess(context.Background(), process)
		server.clearProcess(process)
		return errors.Join(err, stopErr)
	}
	return nil
}

// Restart replaces the child process without closing the supervisor. Stop and
// start are one serialized lifecycle operation, so concurrent restart calls
// cannot overlap child trees and Close can prevent a replacement from being
// resurrected after terminal shutdown.
func (server *localDevServer) Restart(ctx context.Context) error {
	if server == nil || server.endpoint == nil {
		panic("dacode: initialized local development server is required")
	}
	server.lifecycle.Lock()
	defer server.lifecycle.Unlock()
	server.stateMu.Lock()
	if server.closed {
		server.stateMu.Unlock()
		return errLocalDevClosed
	}
	process := server.process
	server.stateMu.Unlock()
	if process != nil {
		if err := server.stopProcess(ctx, process); err != nil {
			return fmt.Errorf("restart local development server: %w", err)
		}
		server.clearProcess(process)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := server.startLocked(ctx); err != nil {
		return fmt.Errorf("restart local development server: %w", err)
	}
	return nil
}

func (server *localDevServer) waitReady(ctx context.Context, process localDevProcess) error {
	var lastProbeError string
	for {
		select {
		case <-process.Done():
			return server.crashError(process)
		default:
		}
		probeContext, cancel := context.WithTimeout(ctx, server.config.ProbeTimeout)
		ready, err := server.config.probe(probeContext, server.endpoint)
		cancel()
		if ready {
			select {
			case <-process.Done():
				return server.crashError(process)
			default:
				return nil
			}
		}
		if err != nil {
			lastProbeError = sanitizeLocalDevDiagnostic(err.Error(), 512)
		}
		if err := server.config.sleep(ctx, server.config.PollInterval); err != nil {
			if cause := context.Cause(ctx); cause != nil {
				if errors.Is(cause, context.DeadlineExceeded) && lastProbeError != "" {
					return fmt.Errorf("local development server did not become ready: %s", lastProbeError)
				}
				return cause
			}
			return err
		}
	}
}

func (server *localDevServer) childEnvironment() []string {
	values := make(map[string]string, len(server.config.Environment)+len(server.config.InheritEnvironment))
	for _, key := range server.config.InheritEnvironment {
		if value, exists := server.config.lookupEnvironment(key); exists && len(value) <= maxLocalDevEnvironmentValue && strings.IndexByte(value, 0) < 0 {
			values[key] = value
		}
	}
	for key, value := range server.config.Environment {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func (server *localDevServer) Close(ctx context.Context) error {
	if server == nil || server.endpoint == nil {
		panic("dacode: initialized local development server is required")
	}
	server.stateMu.Lock()
	if server.closed {
		done := server.closeDone
		server.stateMu.Unlock()
		select {
		case <-done:
			server.stateMu.Lock()
			err := server.closeErr
			server.stateMu.Unlock()
			return err
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	server.closed = true
	if server.starting != nil {
		server.starting()
	}
	server.stateMu.Unlock()

	server.lifecycle.Lock()
	defer server.lifecycle.Unlock()
	server.stateMu.Lock()
	process := server.process
	server.stateMu.Unlock()
	var closeErr error
	if process != nil {
		closeErr = server.stopProcess(ctx, process)
		server.clearProcess(process)
	}
	server.stateMu.Lock()
	server.closeErr = closeErr
	close(server.closeDone)
	server.stateMu.Unlock()
	return closeErr
}

func (server *localDevServer) clearProcess(process localDevProcess) {
	server.stateMu.Lock()
	if server.process == process {
		server.process = nil
	}
	server.stateMu.Unlock()
}

func (server *localDevServer) stopProcess(ctx context.Context, process localDevProcess) error {
	select {
	case <-process.Done():
		return nil
	default:
	}
	signalErr := process.SignalTree()
	if waitLocalDevProcess(ctx, process.Done(), server.config.ShutdownTimeout) {
		return nil
	}
	killErr := process.KillTree()
	if waitLocalDevProcess(context.Background(), process.Done(), server.config.KillTimeout) {
		return errors.Join(sanitizeLocalDevProcessError("signal process tree", signalErr), sanitizeLocalDevProcessError("kill process tree", killErr))
	}
	return errors.Join(
		sanitizeLocalDevProcessError("signal process tree", signalErr),
		sanitizeLocalDevProcessError("kill process tree", killErr),
		errors.New("local development server process tree did not exit within the shutdown bound"),
	)
}

func waitLocalDevProcess(ctx context.Context, done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func sanitizeLocalDevProcessError(operation string, err error) error {
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return fmt.Errorf("%s: %s", operation, sanitizeLocalDevDiagnostic(err.Error(), 512))
}

type localDevServerCrashError struct {
	Cause   error
	LogTail string
}

func (failure *localDevServerCrashError) Error() string {
	if failure == nil {
		return "local development server exited"
	}
	result := "local development server exited"
	if failure.Cause != nil {
		result += ": " + sanitizeLocalDevDiagnostic(failure.Cause.Error(), 512)
	}
	if failure.LogTail != "" {
		result += "\n" + failure.LogTail
	}
	return result
}

func (failure *localDevServerCrashError) Unwrap() error { return failure.Cause }

func (server *localDevServer) crashError(process localDevProcess) error {
	var cause error
	if waitErr := process.WaitError(); waitErr != nil {
		cause = errors.New(sanitizeLocalDevDiagnostic(waitErr.Error(), 512))
	}
	return &localDevServerCrashError{Cause: cause, LogTail: sanitizeLocalDevDiagnostic(server.log.String(), server.config.LogBytes)}
}

type localDevLog struct {
	mu       sync.Mutex
	capacity int
	buffer   []byte
}

func newLocalDevLog(capacity int) *localDevLog {
	if capacity < 1 || capacity > maxLocalDevLogBytes {
		panic("dacode: local development server log bound is out of range")
	}
	return &localDevLog{capacity: capacity}
}

func (log *localDevLog) Write(value []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	written := len(value)
	if len(value) >= log.capacity {
		log.buffer = append(log.buffer[:0], value[len(value)-log.capacity:]...)
		return written, nil
	}
	overflow := len(log.buffer) + len(value) - log.capacity
	if overflow > 0 {
		copy(log.buffer, log.buffer[overflow:])
		log.buffer = log.buffer[:len(log.buffer)-overflow]
	}
	log.buffer = append(log.buffer, value...)
	return written, nil
}

func (log *localDevLog) String() string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return strings.ToValidUTF8(string(log.buffer), "�")
}

func (log *localDevLog) Reset() {
	log.mu.Lock()
	log.buffer = log.buffer[:0]
	log.mu.Unlock()
}

func sanitizeLocalDevDiagnostic(value string, limit int) string {
	value = localDevBearerSecret.ReplaceAllString(value, "Bearer [REDACTED]")
	value = localDevSecretAssignment.ReplaceAllString(value, "$1$2[REDACTED]")
	value = localDevURLUserinfo.ReplaceAllString(value, "$1[REDACTED]@")
	value = localDevKnownSecret.ReplaceAllString(value, "[REDACTED]")
	value = unicodesecurity.RenderTerminalSafe(value)
	value = strings.TrimSpace(value)
	if len(value) > limit {
		value = value[len(value)-limit:]
		for len(value) > 0 && !utf8.ValidString(value) {
			value = value[1:]
		}
	}
	return value
}

func startLocalDevExecProcess(executable string, arguments []string, launch localDevProcessLaunch) (localDevProcess, error) {
	command := exec.Command(executable, arguments...)
	command.Dir = launch.Directory
	command.Env = append([]string(nil), launch.Environment...)
	command.Stdout, command.Stderr = launch.Output, launch.Output
	configureLocalDevCommand(command)
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &localDevExecProcess{command: command, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		process.mu.Lock()
		process.err = err
		process.mu.Unlock()
		close(process.done)
	}()
	return process, nil
}

type localDevExecProcess struct {
	command *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
	err     error
}

func (process *localDevExecProcess) Done() <-chan struct{} { return process.done }

func (process *localDevExecProcess) WaitError() error {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.err
}

func (process *localDevExecProcess) SignalTree() error {
	return signalLocalDevProcessTree(process.command, false)
}
func (process *localDevExecProcess) KillTree() error {
	return signalLocalDevProcessTree(process.command, true)
}

func sleepLocalDevServer(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func probeLocalDevEndpoint(ctx context.Context, endpoint *url.URL) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return false, err
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: (&net.Dialer{Timeout: defaultLocalDevProbeTimeout, KeepAlive: -1}).DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("readiness redirects are disabled") },
	}
	response, err := client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<10))
	return response.StatusCode == http.StatusOK, nil
}

var _ io.Writer = (*localDevLog)(nil)
