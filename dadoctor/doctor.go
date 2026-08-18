// Package dadoctor collects bounded, offline diagnostics suitable for pasting
// into support reports. It reports whether credentials exist, never their
// values, and reduces configured service URLs to origins.
package dadoctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultCommandTimeout = 2 * time.Second
	defaultCommandBytes   = 256
	defaultValueBytes     = 4096
)

// Item is one paste-safe diagnostic fact.
type Item struct {
	Label string `json:"label"`
	Value string `json:"value"`
	OK    bool   `json:"ok"`
}

// Section is a stable group of diagnostic facts.
type Section struct {
	Title   string `json:"title"`
	Healthy bool   `json:"ok"`
	Items   []Item `json:"items"`
}

// OK reports whether every item in the section is healthy.
func (section Section) OK() bool {
	for _, item := range section.Items {
		if !item.OK {
			return false
		}
	}
	return true
}

// Report is the complete offline diagnostic snapshot.
type Report struct {
	Healthy  bool      `json:"healthy"`
	Sections []Section `json:"sections"`
}

// Command is a bounded subprocess request issued by Doctor. The built-in
// collector uses only an absolute git executable and fixed arguments.
type Command struct {
	Executable     string
	Arguments      []string
	Directory      string
	Timeout        time.Duration
	MaxOutputBytes int
}

// System is the host boundary used by Doctor. Supplying it positionally makes
// filesystem, environment, platform, and subprocess authority explicit.
type System interface {
	Platform() (goos, goarch, goVersion string)
	Executable() (string, error)
	UserConfigDir() (string, error)
	LookupEnv(string) (string, bool)
	Stat(string) (os.FileInfo, error)
	LookPath(string) (string, error)
	Run(context.Context, Command) (string, error)
}

// Options configures diagnostic locations and finite bounds. Zero values use
// the current working directory, the platform config directory, a two-second
// git timeout, 256 command-output bytes, and 4 KiB values.
type Options struct {
	WorkingDirectory      string
	ConfigPath            string
	DataDirectory         string
	CredentialEnvironment []string
	CredentialFiles       []string
	EndpointEnvironment   string
	RuntimeVersions       []Version
	Commit                string
	CommandTimeout        time.Duration
	MaxCommandOutputBytes int
	MaxValueBytes         int
}

// Version is an additional runtime dependency version shown in Diagnostics.
type Version struct {
	Name  string
	Value string
}

// Doctor is a reusable offline diagnostic collector.
type Doctor struct {
	application string
	version     string
	system      System
	options     Options
}

// New constructs a Doctor. Required inputs are positional and static invalid
// inputs panic; collection-time host failures become diagnostic facts.
func New(application, version string, system System, options Options) *Doctor {
	if strings.TrimSpace(application) == "" {
		panic("doctor application is required")
	}
	if strings.TrimSpace(version) == "" {
		panic("doctor version is required")
	}
	if nilSystem(system) {
		panic("doctor system is required")
	}
	if options.CommandTimeout < 0 || options.MaxCommandOutputBytes < 0 || options.MaxValueBytes < 0 {
		panic("doctor bounds cannot be negative")
	}
	if options.CommandTimeout == 0 {
		options.CommandTimeout = defaultCommandTimeout
	}
	if options.MaxCommandOutputBytes == 0 {
		options.MaxCommandOutputBytes = defaultCommandBytes
	}
	if options.MaxValueBytes == 0 {
		options.MaxValueBytes = defaultValueBytes
	}
	if options.WorkingDirectory == "" {
		options.WorkingDirectory = "."
	}
	if len(options.CredentialEnvironment) == 0 {
		options.CredentialEnvironment = []string{"OPENAI_API_KEY"}
	}
	if options.EndpointEnvironment == "" {
		options.EndpointEnvironment = "OPENAI_BASE_URL"
	}
	options.CredentialEnvironment = append([]string(nil), options.CredentialEnvironment...)
	options.CredentialFiles = append([]string(nil), options.CredentialFiles...)
	options.RuntimeVersions = append([]Version(nil), options.RuntimeVersions...)
	return &Doctor{application: strings.TrimSpace(application), version: strings.TrimSpace(version), system: system, options: options}
}

func nilSystem(system System) bool {
	if system == nil {
		return true
	}
	value := reflect.ValueOf(system)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Collect gathers diagnostics without network access.
func (doctor *Doctor) Collect(ctx context.Context) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	goos, goarch, goVersion := doctor.system.Platform()
	installPath, installMethod := doctor.installation()
	commit := doctor.commit(ctx)
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	diagnosticItems := []Item{
		doctor.item(doctor.application, doctor.version, true),
	}
	for _, version := range doctor.options.RuntimeVersions {
		if strings.TrimSpace(version.Name) != "" && strings.TrimSpace(version.Value) != "" {
			diagnosticItems = append(diagnosticItems, doctor.item(version.Name, version.Value, true))
		}
	}
	diagnosticItems = append(diagnosticItems,
		doctor.item("Commit hash", commit, true),
		doctor.item("Go", goVersion, true),
		doctor.item("Platform", platformTag(goos, goarch), true),
		doctor.item("Install method", installMethod, true),
		doctor.item("Path", installPath, true),
	)
	sections := []Section{{Title: "Diagnostics", Items: diagnosticItems}, doctor.updatesSection(), doctor.providerSection(), doctor.configurationSection()}
	report := Report{Healthy: true, Sections: sections}
	for index := range report.Sections {
		report.Sections[index].Healthy = report.Sections[index].OK()
		if !report.Sections[index].Healthy {
			report.Healthy = false
		}
	}
	return report, nil
}

func (doctor *Doctor) updatesSection() Section {
	return Section{Title: "Updates", Items: []Item{
		doctor.item("Update checks", "manual signed channel only", true),
		doctor.item("Auto-updates", "disabled", true),
		doctor.item("Latest version", "not checked (doctor is offline)", true),
		doctor.item("Last checked", "never", true),
	}}
}

func (doctor *Doctor) installation() (string, string) {
	executable, err := doctor.system.Executable()
	if err != nil || strings.TrimSpace(executable) == "" {
		return "unknown", "unknown"
	}
	method := "binary"
	if doctor.version == "development" {
		method = "development"
	}
	return filepath.Dir(executable), method
}

var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

func (doctor *Doctor) commit(ctx context.Context) string {
	if commitPattern.MatchString(doctor.options.Commit) {
		return shortCommit(doctor.options.Commit)
	}
	git, err := doctor.system.LookPath("git")
	if err != nil {
		return "unknown"
	}
	git, err = filepath.Abs(git)
	if err != nil {
		return "unknown"
	}
	output, err := doctor.system.Run(ctx, Command{Executable: git, Arguments: []string{"rev-parse", "--short=12", "HEAD"}, Directory: doctor.options.WorkingDirectory, Timeout: doctor.options.CommandTimeout, MaxOutputBytes: doctor.options.MaxCommandOutputBytes})
	if err != nil {
		return "unknown"
	}
	output = strings.TrimSpace(output)
	if !commitPattern.MatchString(output) {
		return "unknown"
	}
	return shortCommit(output)
}

func shortCommit(value string) string {
	value = strings.ToLower(value)
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func platformTag(goos, goarch string) string {
	goos, goarch = strings.ToLower(strings.TrimSpace(goos)), strings.ToLower(strings.TrimSpace(goarch))
	if goos == "" {
		goos = "unknown"
	}
	if goarch == "" {
		goarch = "unknown"
	}
	return goos + "-" + goarch
}

func (doctor *Doctor) providerSection() Section {
	configured := false
	credentialOK := true
	for _, name := range doctor.options.CredentialEnvironment {
		if value, ok := doctor.system.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
			configured = true
			break
		}
	}
	if !configured {
		for _, credentialPath := range doctor.options.CredentialFiles {
			if _, err := doctor.system.Stat(credentialPath); err == nil {
				configured = true
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				credentialOK = false
			}
		}
	}
	endpoint := "default"
	if value, ok := doctor.system.LookupEnv(doctor.options.EndpointEnvironment); ok && strings.TrimSpace(value) != "" {
		endpoint = sanitizeEndpoint(value)
	}
	return Section{Title: "Provider", Items: []Item{
		doctor.item("Credentials", credentialStatus(configured, credentialOK), credentialOK),
		doctor.item("Endpoint", endpoint, true),
	}}
}

func credentialStatus(configured, ok bool) string {
	if configured {
		return "configured"
	}
	if !ok {
		return "unreadable"
	}
	return "not set"
}

func sanitizeEndpoint(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return "custom endpoint configured"
	}
	host := parsed.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := parsed.Port(); port != "" {
		host += ":" + port
	}
	return strings.ToLower(parsed.Scheme) + "://" + host
}

func (doctor *Doctor) configurationSection() Section {
	configDir, configErr := doctor.system.UserConfigDir()
	configPath, dataDir := doctor.options.ConfigPath, doctor.options.DataDirectory
	if configErr == nil {
		if dataDir == "" {
			dataDir = filepath.Join(configDir, doctor.application)
		}
		if configPath == "" {
			configPath = filepath.Join(dataDir, "config.json")
		}
	}
	items := []Item{}
	if configErr != nil && (configPath == "" || dataDir == "") {
		items = append(items, doctor.item("Data directory", "unknown (unreadable)", false), doctor.item("Config file", "unknown (unreadable)", false))
	} else {
		items = append(items, doctor.pathItem("Data directory", dataDir), doctor.pathItem("Config file", configPath))
	}
	return Section{Title: "Configuration", Items: items}
}

func (doctor *Doctor) pathItem(label, value string) Item {
	suffix, ok := "exists", true
	if _, err := doctor.system.Stat(value); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			suffix = "not created"
		} else {
			suffix, ok = "unreadable", false
		}
	}
	return doctor.item(label, fmt.Sprintf("%s (%s)", value, suffix), ok)
}

func (doctor *Doctor) item(label, value string, ok bool) Item {
	return Item{Label: boundSafe(label, doctor.options.MaxValueBytes), Value: boundSafe(value, doctor.options.MaxValueBytes), OK: ok}
}

func boundSafe(value string, limit int) string {
	var builder strings.Builder
	for _, r := range value {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			fmt.Fprintf(&builder, "\\u%04x", r)
		} else {
			builder.WriteRune(r)
		}
		if builder.Len() >= limit {
			break
		}
	}
	value = builder.String()
	if len(value) <= limit {
		return value
	}
	marker := "...[truncated]"
	value = value[:max(0, limit-len(marker))]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + marker
}

// WriteText renders a stable tree. ASCII selects portable connectors.
func WriteText(output io.Writer, report Report, ascii bool) error {
	tee, corner := "├", "└"
	if ascii {
		tee, corner = "|-", "`-"
	}
	for _, section := range report.Sections {
		status := "ok"
		if !section.OK() {
			status = "warning"
		}
		if _, err := fmt.Fprintf(output, "%s [%s]\n", section.Title, status); err != nil {
			return err
		}
		for index, item := range section.Items {
			connector := tee
			if index == len(section.Items)-1 {
				connector = corner
			}
			if _, err := fmt.Fprintf(output, "%s %s: %s\n", connector, item.Label, item.Value); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteJSON renders the stable version-one doctor envelope.
func WriteJSON(output io.Writer, report Report) error {
	return json.NewEncoder(output).Encode(map[string]any{"schema_version": 1, "command": "doctor", "data": report})
}

type osSystem struct{}

// OSSystem returns the ordinary local host implementation.
func OSSystem() System { return osSystem{} }
func (osSystem) Platform() (string, string, string) {
	return runtime.GOOS, runtime.GOARCH, runtime.Version()
}
func (osSystem) Executable() (string, error)           { return os.Executable() }
func (osSystem) UserConfigDir() (string, error)        { return os.UserConfigDir() }
func (osSystem) LookupEnv(name string) (string, bool)  { return os.LookupEnv(name) }
func (osSystem) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }
func (osSystem) LookPath(name string) (string, error)  { return exec.LookPath(name) }
func (osSystem) Run(ctx context.Context, command Command) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()
	process := exec.CommandContext(runCtx, command.Executable, command.Arguments...)
	process.Dir = command.Directory
	buffer := &limitedBuffer{limit: command.MaxOutputBytes}
	process.Stdout, process.Stderr = buffer, io.Discard
	if err := process.Run(); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

type limitedBuffer struct {
	value []byte
	limit int
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	accepted := min(len(value), max(0, buffer.limit-len(buffer.value)))
	buffer.value = append(buffer.value, value[:accepted]...)
	return len(value), nil
}
func (buffer *limitedBuffer) String() string { return string(buffer.value) }

// BuildCommit returns the VCS revision embedded by the Go toolchain, if valid.
func BuildCommit() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && commitPattern.MatchString(setting.Value) {
				return setting.Value
			}
		}
	}
	return ""
}
