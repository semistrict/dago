package dadoctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeSystem struct {
	goos, goarch, goVersion string
	executable, configDir   string
	env                     map[string]string
	stats                   map[string]error
	gitPath, gitOutput      string
	gitErr                  error
	command                 Command
}

func (system *fakeSystem) Platform() (string, string, string) {
	return system.goos, system.goarch, system.goVersion
}
func (system *fakeSystem) Executable() (string, error)    { return system.executable, nil }
func (system *fakeSystem) UserConfigDir() (string, error) { return system.configDir, nil }
func (system *fakeSystem) LookupEnv(name string) (string, bool) {
	value, ok := system.env[name]
	return value, ok
}
func (system *fakeSystem) Stat(path string) (os.FileInfo, error) { return nil, system.stats[path] }
func (system *fakeSystem) LookPath(string) (string, error) {
	if system.gitPath == "" {
		return "", os.ErrNotExist
	}
	return system.gitPath, nil
}
func (system *fakeSystem) Run(_ context.Context, command Command) (string, error) {
	system.command = command
	return system.gitOutput, system.gitErr
}

func healthySystem() *fakeSystem {
	return &fakeSystem{goos: "linux", goarch: "amd64", goVersion: "go1.26", executable: "/opt/dacode/dacode", configDir: "/config", env: map[string]string{}, stats: map[string]error{"/config/dacode": os.ErrNotExist, "/config/dacode/config.json": os.ErrNotExist}, gitPath: "/usr/bin/git", gitOutput: "abcdef123456\n"}
}

func TestNewRejectsTypedNilSystem(t *testing.T) {
	var system *fakeSystem
	defer func() {
		if recover() == nil {
			t.Fatal("typed-nil system did not panic")
		}
	}()
	New("dacode", "development", system, Options{})
}

func TestDoctorCollectsStableOfflineSections(t *testing.T) {
	system := healthySystem()
	doctor := New("dacode", "1.2.3", system, Options{})
	report, err := doctor.Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Healthy {
		t.Fatalf("report = %#v", report)
	}
	titles := []string{}
	for _, section := range report.Sections {
		titles = append(titles, section.Title)
	}
	if !reflect.DeepEqual(titles, []string{"Diagnostics", "Updates", "Provider", "Configuration"}) {
		t.Fatalf("titles = %#v", titles)
	}
	if system.command.Executable != "/usr/bin/git" || !reflect.DeepEqual(system.command.Arguments, []string{"rev-parse", "--short=12", "HEAD"}) || system.command.Timeout != 2*time.Second || system.command.MaxOutputBytes != 256 {
		t.Fatalf("git command = %#v", system.command)
	}
}

func TestDoctorNeverReportsCredentialOrEndpointSecrets(t *testing.T) {
	system := healthySystem()
	system.env["OPENAI_API_KEY"] = "private-provider-value"
	system.env["OPENAI_BASE_URL"] = fmt.Sprintf("https://%s:%s@%s:8443/v1?api_key=private-query#fragment", "user", "private-userinfo", "example.com")
	report, err := New("dacode", "development", system, Options{}).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{"private-provider-value", "private-userinfo", "api_key", "fragment", "/v1"} {
		if strings.Contains(text, secret) {
			t.Fatalf("report leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "configured") || !strings.Contains(text, "https://example.com:8443") {
		t.Fatalf("report = %s", text)
	}
}

func TestDoctorHandlesMalformedEndpointAndSavedCredentialPresence(t *testing.T) {
	system := healthySystem()
	system.env["OPENAI_BASE_URL"] = "http://[::1"
	system.stats["/config/oauth.json"] = nil
	report, err := New("dacode", "development", system, Options{CredentialFiles: []string{"/config/oauth.json"}}).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(report)
	if !strings.Contains(string(encoded), `"value":"configured"`) || !strings.Contains(string(encoded), "custom endpoint configured") {
		t.Fatalf("report = %s", encoded)
	}
}

func TestDoctorMarksUnreadableSavedCredentialUnhealthyWithoutPrintingPath(t *testing.T) {
	system := healthySystem()
	system.stats["/private/credential.json"] = errors.New("permission denied")
	report, err := New("dacode", "development", system, Options{CredentialFiles: []string{"/private/credential.json"}}).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(report)
	if report.Healthy || !strings.Contains(string(encoded), `"value":"unreadable"`) || strings.Contains(string(encoded), "/private/credential.json") {
		t.Fatalf("report = %s", encoded)
	}
}

func TestDoctorBoundsAndEscapesUntrustedValues(t *testing.T) {
	system := healthySystem()
	system.executable = "/tmp/" + strings.Repeat("x", 200) + "\x1b[31m/dacode"
	report, err := New("dacode", "development", system, Options{Commit: "abcdef123456", MaxValueBytes: 64}).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(report)
	if bytes.Contains(encoded, []byte{0x1b}) {
		t.Fatalf("control byte leaked: %q", encoded)
	}
	for _, section := range report.Sections {
		for _, item := range section.Items {
			if len(item.Value) > 64 {
				t.Fatalf("unbounded value %q", item.Value)
			}
		}
	}
}

func TestDoctorMarksUnreadablePathUnhealthyButMissingPathHealthy(t *testing.T) {
	system := healthySystem()
	system.stats["/config/dacode"] = errors.New("permission denied")
	report, err := New("dacode", "development", system, Options{}).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Healthy {
		t.Fatalf("report = %#v", report)
	}
	configuration := report.Sections[3]
	if configuration.Items[0].OK || !strings.Contains(configuration.Items[0].Value, "unreadable") || !configuration.Items[1].OK {
		t.Fatalf("configuration = %#v", configuration)
	}
}

func TestPlatformTagsAreCrossPlatform(t *testing.T) {
	for _, test := range []struct{ goos, arch, want string }{{"windows", "amd64", "windows-amd64"}, {"darwin", "arm64", "darwin-arm64"}, {"linux", "riscv64", "linux-riscv64"}, {"", "", "unknown-unknown"}} {
		if got := platformTag(test.goos, test.arch); got != test.want {
			t.Errorf("platformTag(%q,%q) = %q", test.goos, test.arch, got)
		}
	}
}

func TestDoctorJSONAndTextRendering(t *testing.T) {
	report, err := New("dacode", "development", healthySystem(), Options{}).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := WriteJSON(&output, report); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          Report `json:"data"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 || envelope.Command != "doctor" || !envelope.Data.Healthy {
		t.Fatalf("envelope = %#v", envelope)
	}
	output.Reset()
	if err := WriteText(&output, report, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Diagnostics [ok]") || !strings.Contains(output.String(), "|- ") || !strings.Contains(output.String(), "`- ") {
		t.Fatalf("text = %q", output.String())
	}
}

func TestDoctorHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := New("dacode", "development", healthySystem(), Options{}).Collect(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
