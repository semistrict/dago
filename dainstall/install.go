// Package dainstall provides a closed-catalog dependency installer. It never
// constructs shell commands: applications register fixed executable/argument
// vectors and callers must grant explicit authorization before mutation.
package dainstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout  = 5 * time.Minute
	defaultLockWait = 5 * time.Second
	maxTimeout      = 30 * time.Minute
	maxLockWait     = time.Minute
	maxSpecs        = 128
	maxArguments    = 64
	maxArgumentLen  = 4096
)

// Kind identifies a curated optional integration or a curated external
// package command. Arbitrary package names are never accepted.
type Kind string

const (
	Extra   Kind = "extra"
	Package Kind = "package"
)

// Authorization is the explicit capability required for an install that
// launches a process.
type Authorization string

const (
	AuthorizationDenied  Authorization = ""
	AuthorizationGranted Authorization = "install-approved"
)

// Status is the outcome of a successful catalog lookup.
type Status string

const (
	AlreadyAvailable Status = "already_available"
	Installed        Status = "installed"
)

var (
	ErrUnknownDependency = errors.New("dependency is not in the install allowlist")
	ErrAuthorization     = errors.New("dependency install requires explicit authorization")
	ErrInstallFailed     = errors.New("dependency install failed")
	ErrInvalidKind       = errors.New("invalid dependency kind")
)

var namePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)

// Spec is one trusted catalog entry. BuiltIn entries report availability
// without process authority. Other entries execute the fixed command vector.
type Spec struct {
	Name        string
	Kind        Kind
	Description string
	BuiltIn     bool
	Executable  string
	Arguments   []string
}

// Entry is the safe public projection of a catalog entry.
type Entry struct {
	Name        string `json:"name"`
	Kind        Kind   `json:"kind"`
	Description string `json:"description,omitempty"`
	BuiltIn     bool   `json:"built_in"`
}

// Result is a successful install or availability check.
type Result struct {
	Name   string `json:"name"`
	Kind   Kind   `json:"kind"`
	Status Status `json:"status"`
}

// Command is the finite direct-execution request sent to Executor.
type Command struct {
	Executable string
	Arguments  []string
	Timeout    time.Duration
	LockPath   string
	LockWait   time.Duration
}

// Executor is the explicit host-process authority used by Installer.
type Executor interface {
	LookPath(string) (string, error)
	Run(context.Context, Command) error
}

// Options configures finite execution and cross-process serialization. Zero
// selects a five-minute execution timeout and five-second lock wait. LockPath
// should be in an application-private directory; zero uses the user cache.
type Options struct {
	Timeout  time.Duration
	LockWait time.Duration
	LockPath string
}

// Installer resolves a closed immutable catalog.
type Installer struct {
	executor Executor
	options  Options
	specs    map[Kind]map[string]Spec
}

// New constructs an Installer. Required authority and catalog inputs are
// positional. Static invalid definitions panic; runtime failures are errors.
func New(executor Executor, catalog []Spec, options Options) *Installer {
	if nilExecutor(executor) {
		panic("dependency installer executor is required")
	}
	if len(catalog) > maxSpecs {
		panic("dependency installer catalog exceeds 128 entries")
	}
	if options.Timeout < 0 || options.Timeout > maxTimeout {
		panic("dependency installer timeout must be between zero and 30 minutes")
	}
	if options.LockWait < 0 || options.LockWait > maxLockWait {
		panic("dependency installer lock wait must be between zero and one minute")
	}
	if options.Timeout == 0 {
		options.Timeout = defaultTimeout
	}
	if options.LockWait == 0 {
		options.LockWait = defaultLockWait
	}
	if options.LockPath == "" {
		options.LockPath = defaultLockPath()
	}
	if !filepath.IsAbs(options.LockPath) {
		panic("dependency installer lock path must be absolute")
	}
	installer := &Installer{executor: executor, options: options, specs: map[Kind]map[string]Spec{Extra: {}, Package: {}}}
	for _, raw := range catalog {
		spec := raw
		spec.Name = strings.ToLower(strings.TrimSpace(spec.Name))
		if !namePattern.MatchString(spec.Name) {
			panic(fmt.Sprintf("dependency installer invalid name %q", raw.Name))
		}
		if spec.Kind != Extra && spec.Kind != Package {
			panic(fmt.Sprintf("dependency installer invalid kind %q", spec.Kind))
		}
		if _, duplicate := installer.specs[spec.Kind][spec.Name]; duplicate {
			panic(fmt.Sprintf("dependency installer duplicate %s %q", spec.Kind, spec.Name))
		}
		if len(spec.Arguments) > maxArguments {
			panic(fmt.Sprintf("dependency installer %q has too many arguments", spec.Name))
		}
		for _, argument := range spec.Arguments {
			if len(argument) > maxArgumentLen || strings.ContainsRune(argument, 0) {
				panic(fmt.Sprintf("dependency installer %q has an invalid argument", spec.Name))
			}
		}
		if spec.BuiltIn {
			if spec.Executable != "" || len(spec.Arguments) != 0 {
				panic(fmt.Sprintf("dependency installer built-in %q cannot define a command", spec.Name))
			}
		} else if strings.TrimSpace(spec.Executable) == "" || strings.ContainsRune(spec.Executable, 0) {
			panic(fmt.Sprintf("dependency installer %q requires an executable", spec.Name))
		}
		if !spec.BuiltIn && !filepath.IsAbs(spec.Executable) && filepath.Base(spec.Executable) != spec.Executable {
			panic(fmt.Sprintf("dependency installer %q executable must be a bare name or absolute path", spec.Name))
		}
		spec.Description = safeDescription(spec.Description)
		spec.Arguments = slices.Clone(spec.Arguments)
		installer.specs[spec.Kind][spec.Name] = spec
	}
	return installer
}

func nilExecutor(executor Executor) bool {
	if executor == nil {
		return true
	}
	value := reflect.ValueOf(executor)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func safeDescription(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	if len(value) > 256 {
		value = value[:256]
	}
	return value
}

// Available returns a sorted, authority-free catalog projection.
func (installer *Installer) Available(kind Kind) []Entry {
	entries := []Entry{}
	for _, spec := range installer.specs[kind] {
		entries = append(entries, Entry{Name: spec.Name, Kind: spec.Kind, Description: spec.Description, BuiltIn: spec.BuiltIn})
	}
	slices.SortFunc(entries, func(left, right Entry) int { return strings.Compare(left.Name, right.Name) })
	return entries
}

// Install checks one allowlisted dependency. Process-backed entries require
// AuthorizationGranted. User input selects a catalog entry but never becomes
// an executable, argument, environment variable, or shell fragment.
func (installer *Installer) Install(ctx context.Context, kind Kind, name string, authorization Authorization) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if kind != Extra && kind != Package {
		return Result{}, ErrInvalidKind
	}
	name = strings.ToLower(strings.TrimSpace(name))
	spec, ok := installer.specs[kind][name]
	if !ok {
		return Result{}, fmt.Errorf("%w: %s %q", ErrUnknownDependency, kind, safeName(name))
	}
	if spec.BuiltIn {
		return Result{Name: spec.Name, Kind: spec.Kind, Status: AlreadyAvailable}, nil
	}
	if authorization != AuthorizationGranted {
		return Result{}, ErrAuthorization
	}
	executable := spec.Executable
	if !filepath.IsAbs(executable) {
		resolved, err := installer.executor.LookPath(executable)
		if err != nil {
			return Result{}, fmt.Errorf("%w: installer executable unavailable", ErrInstallFailed)
		}
		executable, err = filepath.Abs(resolved)
		if err != nil {
			return Result{}, fmt.Errorf("%w: installer executable unavailable", ErrInstallFailed)
		}
	}
	err := installer.executor.Run(ctx, Command{
		Executable: executable,
		Arguments:  slices.Clone(spec.Arguments),
		Timeout:    installer.options.Timeout,
		LockPath:   installer.options.LockPath,
		LockWait:   installer.options.LockWait,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Result{}, err
		}
		return Result{}, fmt.Errorf("%w: %s %q", ErrInstallFailed, kind, spec.Name)
	}
	return Result{Name: spec.Name, Kind: spec.Kind, Status: Installed}, nil
}

func safeName(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if character >= 0x20 && character <= 0x7e {
			builder.WriteRune(character)
		} else {
			builder.WriteString(strconv.QuoteRuneToASCII(character))
		}
		if builder.Len() > 128 {
			builder.WriteString("...")
			break
		}
	}
	value = builder.String()
	if len(value) > 128 {
		return value[:128] + "..."
	}
	return value
}

func defaultLockPath() string {
	if cache, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cache) {
		return filepath.Join(cache, "dago", "dependency-install.lock")
	}
	return filepath.Join(os.TempDir(), "dago-dependency-install.lock")
}

type osExecutor struct{}

// OSExecutor returns the ordinary direct-process implementation.
func OSExecutor() Executor                              { return osExecutor{} }
func (osExecutor) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (osExecutor) Run(ctx context.Context, command Command) error {
	runCtx, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()
	unlock, err := lockInstall(runCtx, command.LockPath, command.LockWait)
	if err != nil {
		return err
	}
	defer unlock()
	process := exec.CommandContext(runCtx, command.Executable, command.Arguments...)
	configureInstallProcess(process)
	process.Env = installEnvironment()
	process.Stdout, process.Stderr, process.Stdin = nil, nil, nil
	err = process.Run()
	if runCtx.Err() != nil {
		return runCtx.Err()
	}
	return err
}

func installEnvironment() []string {
	allowed := []string{
		"PATH", "HOME", "USERPROFILE", "TMPDIR", "TEMP", "TMP",
		"SystemRoot", "ComSpec", "PATHEXT", "XDG_CACHE_HOME",
		"GOCACHE", "GOMODCACHE", "GOPATH", "GOPROXY", "GOSUMDB",
		"GONOSUMDB", "GOPRIVATE", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"NO_PROXY", "no_proxy",
	}
	result := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if value, found := os.LookupEnv(name); found {
			result = append(result, name+"="+value)
		}
	}
	return result
}
