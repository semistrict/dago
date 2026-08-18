// Package daenv resolves bounded project and user dotenv layers without
// mutating the process environment.
package daenv

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

var alwaysDenied = map[string]struct{}{
	"BASH_ENV": {}, "BASHOPTS": {}, "CDPATH": {}, "COMSPEC": {},
	"DYLD_INSERT_LIBRARIES": {}, "DYLD_LIBRARY_PATH": {}, "ENV": {},
	"GIT_ASKPASS": {}, "GLOBIGNORE": {}, "LD_AUDIT": {},
	"LD_LIBRARY_PATH": {}, "LD_PRELOAD": {}, "NODE_OPTIONS": {}, "PATH": {},
	"PYTHONEXECUTABLE": {}, "PYTHONHOME": {}, "PYTHONPATH": {},
	"PYTHONSTARTUP": {}, "SHELLOPTS": {}, "SSH_ASKPASS": {},
	"SYSTEMROOT": {}, "WINDIR": {}, "DEEPAGENTS_INHERITED_PYTHONPATH": {},
}

var projectDenied = map[string]struct{}{
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "ALL_PROXY": {}, "NO_PROXY": {},
	"http_proxy": {}, "https_proxy": {}, "all_proxy": {}, "no_proxy": {},
	"SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
	"LANGSMITH_ENDPOINT": {}, "LANGCHAIN_ENDPOINT": {},
	"ANTHROPIC_BASE_URL": {}, "AZURE_OPENAI_ENDPOINT": {},
	"FIREWORKS_BASE_URL": {}, "GOOGLE_GEMINI_BASE_URL": {},
	"OPENAI_API_BASE": {}, "OPENAI_BASE_URL": {}, "OPENROUTER_BASE_URL": {},
	"GIT_CONFIG_GLOBAL": {}, "GIT_CONFIG_SYSTEM": {}, "GIT_EXEC_PATH": {},
	"GIT_SSH": {}, "GIT_SSH_COMMAND": {}, "GIT_TEMPLATE_DIR": {},
	"BUNDLE_GEMFILE": {}, "CURL_HOME": {}, "DOTNET_STARTUP_HOOKS": {},
	"JDK_JAVA_OPTIONS": {}, "JAVA_TOOL_OPTIONS": {}, "NPM_CONFIG_USERCONFIG": {},
	"PERL5OPT": {}, "RUBYOPT": {}, "ZDOTDIR": {}, "_JAVA_OPTIONS": {},
	"DEEPAGENTS_CODE_DANGEROUSLY_ENABLE_PROJECT_MCP_SERVERS": {},
	"DEEPAGENTS_CODE_DISABLED_PROJECT_MCP_SERVERS":           {},
	"DEEPAGENTS_CODE_ENABLED_PROJECT_MCP_SERVERS":            {},
	"DEEPAGENTS_CODE_AUTO_CLASSIFIER_MODEL":                  {},
	"DEEPAGENTS_CODE_AUTO_CLASSIFIER_TIMEOUT":                {},
	"TERM_PROGRAM": {},
}

// Options controls optional discovery and finite parsing limits. Its zero value
// discovers the user file at ~/.deepagents/.env and applies conservative bounds.
type Options struct {
	GlobalPath    string
	MaxFileBytes  int64
	MaxLines      int
	MaxKeyBytes   int
	MaxValueBytes int
	MaxTotalBytes int
	MaxAncestors  int
}

// Ignored records one rejected dotenv assignment without retaining its value.
type Ignored struct {
	Path   string
	Key    string
	Reason string
}

// Result is one deterministic, side-effect-free environment resolution.
type Result struct {
	Values      map[string]string
	Environment []string
	ProjectPath string
	GlobalPath  string
	Ignored     []Ignored
}

// Load resolves required start-directory and process-environment inputs using
// shell > nearest project .env > user .env precedence.
func Load(startDir string, environ []string, options Options) (Result, error) {
	if strings.TrimSpace(startDir) == "" || strings.ContainsRune(startDir, 0) {
		panic("daenv: start directory is required")
	}
	options = options.withDefaults()
	start, err := filepath.Abs(startDir)
	if err != nil {
		return Result{}, fmt.Errorf("resolve dotenv start directory: %w", err)
	}
	info, err := os.Stat(start)
	if err != nil || !info.IsDir() {
		return Result{}, errors.New("dotenv start path must be a directory")
	}
	values := make(map[string]string, len(environ))
	seen := make(map[string]struct{}, len(environ))
	for _, entry := range environ {
		if runtime.GOOS == "windows" && strings.HasPrefix(entry, "=") {
			// Windows preserves drive-current-directory pseudo-variables such as
			// =C:=C:\\work. They are not ordinary environment assignments and
			// must never participate in dotenv precedence.
			continue
		}
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			return Result{}, errors.New("process environment contains an invalid entry")
		}
		identity := environmentIdentity(key)
		if _, exists := seen[identity]; exists {
			return Result{}, errors.New("process environment contains a duplicate name")
		}
		values[key] = value
		seen[identity] = struct{}{}
	}
	projectPath, err := findProjectFile(start, options.MaxAncestors)
	if err != nil {
		return Result{}, err
	}
	globalPath := options.GlobalPath
	if globalPath == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr == nil {
			globalPath = filepath.Join(home, ".deepagents", ".env")
		}
	} else if !filepath.IsAbs(globalPath) || strings.ContainsRune(globalPath, 0) {
		panic("daenv: global dotenv path must be absolute")
	}
	result := Result{Values: values, ProjectPath: projectPath}
	if projectPath != "" {
		if err := applyFile(projectPath, true, &result, seen, options); err != nil {
			return Result{}, err
		}
	}
	if globalPath != "" && globalPath != projectPath {
		if exists, err := regularFileExists(globalPath); err != nil {
			return Result{}, err
		} else if exists {
			result.GlobalPath = globalPath
			if err := applyFile(globalPath, false, &result, seen, options); err != nil {
				return Result{}, err
			}
		}
	}
	keys := make([]string, 0, len(result.Values))
	total := 0
	for key, value := range result.Values {
		total += len(key) + len(value) + 1
		if total > options.MaxTotalBytes {
			return Result{}, errors.New("resolved environment exceeds size limit")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result.Environment = make([]string, 0, len(keys))
	for _, key := range keys {
		result.Environment = append(result.Environment, key+"="+result.Values[key])
	}
	return result, nil
}

func (options Options) withDefaults() Options {
	if options.MaxFileBytes == 0 {
		options.MaxFileBytes = 1 << 20
	}
	if options.MaxLines == 0 {
		options.MaxLines = 4096
	}
	if options.MaxKeyBytes == 0 {
		options.MaxKeyBytes = 256
	}
	if options.MaxValueBytes == 0 {
		options.MaxValueBytes = 64 << 10
	}
	if options.MaxTotalBytes == 0 {
		options.MaxTotalBytes = 16 << 20
	}
	if options.MaxAncestors == 0 {
		options.MaxAncestors = 256
	}
	if options.MaxFileBytes < 1 || options.MaxFileBytes > 16<<20 ||
		options.MaxLines < 1 || options.MaxLines > 65536 ||
		options.MaxKeyBytes < 1 || options.MaxKeyBytes > 4096 ||
		options.MaxValueBytes < 1 || options.MaxValueBytes > 1<<20 ||
		options.MaxTotalBytes < 1 || options.MaxTotalBytes > 64<<20 ||
		options.MaxAncestors < 1 || options.MaxAncestors > 4096 {
		panic("daenv: invalid dotenv limits")
	}
	return options
}

func findProjectFile(start string, maxAncestors int) (string, error) {
	current := start
	for depth := 0; depth < maxAncestors; depth++ {
		candidate := filepath.Join(current, ".env")
		exists, err := regularFileExists(candidate)
		if err != nil {
			return "", err
		}
		if exists {
			return candidate, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", nil
		}
		current = parent
	}
	return "", errors.New("dotenv ancestor search exceeded limit")
}

func regularFileExists(filePath string) (bool, error) {
	info, err := os.Lstat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect dotenv file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, errors.New("dotenv path must be a regular non-symlink file")
	}
	return true, nil
}

func applyFile(filePath string, project bool, result *Result, seen map[string]struct{}, options Options) error {
	before, err := os.Lstat(filePath)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() > options.MaxFileBytes {
		return errors.New("dotenv file is invalid or exceeds its size limit")
	}
	file, err := openDotenv(filePath)
	if err != nil {
		return fmt.Errorf("open dotenv file: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return errors.New("dotenv file changed during open")
	}
	limited := &io.LimitedReader{R: file, N: options.MaxFileBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), options.MaxValueBytes+options.MaxKeyBytes+4096)
	parsed := map[string]string{}
	lines := 0
	for scanner.Scan() {
		lines++
		if lines > options.MaxLines {
			return errors.New("dotenv file exceeds line limit")
		}
		key, value, ok, err := parseLine(scanner.Text(), options)
		if err != nil {
			return fmt.Errorf("parse dotenv %s line %d: %w", filePath, lines, err)
		}
		if ok {
			parsed[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read dotenv file: %w", err)
	}
	if limited.N == 0 {
		return errors.New("dotenv file exceeds size limit")
	}
	keys := make([]string, 0, len(parsed))
	for key := range parsed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		reason := ""
		if deniedEnvironmentKey(alwaysDenied, key) {
			reason = "unsafe in dotenv files"
		} else if project {
			if deniedEnvironmentKey(projectDenied, key) || deniedProjectPrefix(key) {
				reason = "reserved for trusted user configuration"
			}
		}
		if reason != "" {
			result.Ignored = append(result.Ignored, Ignored{Path: filePath, Key: key, Reason: reason})
			continue
		}
		identity := environmentIdentity(key)
		if _, exists := seen[identity]; !exists {
			result.Values[key] = parsed[key]
			seen[identity] = struct{}{}
		}
	}
	return nil
}

func environmentIdentity(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

func deniedEnvironmentKey(denied map[string]struct{}, key string) bool {
	if _, exists := denied[key]; exists {
		return true
	}
	if runtime.GOOS == "windows" {
		_, exists := denied[strings.ToUpper(key)]
		return exists
	}
	return false
}

func deniedProjectPrefix(key string) bool {
	identity := key
	if runtime.GOOS == "windows" {
		identity = strings.ToUpper(key)
	}
	return strings.HasPrefix(identity, "GIT_CONFIG_KEY_") ||
		strings.HasPrefix(identity, "GIT_CONFIG_VALUE_")
}

func parseLine(line string, options Options) (string, string, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	if strings.HasPrefix(line, "export ") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	}
	key, raw, ok := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	if !ok || !validKey(key) || len(key) > options.MaxKeyBytes {
		return "", "", false, errors.New("invalid environment assignment")
	}
	raw = strings.TrimSpace(raw)
	value, err := parseValue(raw)
	if err != nil || len(value) > options.MaxValueBytes || strings.ContainsRune(value, 0) {
		return "", "", false, errors.New("invalid or oversized environment value")
	}
	return key, value, true, nil
}

func validKey(key string) bool {
	for index, value := range []byte(key) {
		if value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || index > 0 && value >= '0' && value <= '9' {
			continue
		}
		return false
	}
	return key != ""
}

func parseValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if raw[0] == '\'' {
		end := strings.Index(raw[1:], "'")
		if end < 0 || strings.TrimSpace(raw[end+2:]) != "" && !strings.HasPrefix(strings.TrimSpace(raw[end+2:]), "#") {
			return "", errors.New("unterminated quoted value")
		}
		return raw[1 : end+1], nil
	}
	if raw[0] == '"' {
		for index := 1; index < len(raw); index++ {
			if raw[index] != '"' || escaped(raw, index) {
				continue
			}
			if tail := strings.TrimSpace(raw[index+1:]); tail != "" && !strings.HasPrefix(tail, "#") {
				return "", errors.New("unexpected text after quoted value")
			}
			return strconv.Unquote(raw[:index+1])
		}
		return "", errors.New("unterminated quoted value")
	}
	if index := strings.Index(raw, " #"); index >= 0 {
		raw = raw[:index]
	}
	return strings.TrimSpace(raw), nil
}

func escaped(value string, index int) bool {
	count := 0
	for index--; index >= 0 && value[index] == '\\'; index-- {
		count++
	}
	return count%2 == 1
}
