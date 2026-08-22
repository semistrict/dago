package dacode

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestACPDiagnosticLogRecordsStagesWithoutSecrets(t *testing.T) {
	stateDirectory := t.TempDir()
	log, err := openACPDiagnosticLog(stateDirectory)
	if err != nil {
		t.Fatalf("openACPDiagnosticLog error = %v", err)
	}
	log.clock = func() time.Time { return time.Date(2026, time.August, 22, 18, 0, 0, 0, time.UTC) }
	log.Event("session.new.start", "model=gpt-test")
	log.Failure("session.new.auth.failed", errors.New("token=must-not-appear"))
	if err := log.Close(); err != nil {
		t.Fatalf("Close error = %v", err)
	}

	payload, err := os.ReadFile(filepath.Join(stateDirectory, acpDiagnosticLogFilename))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	text := string(payload)
	for _, expected := range []string{"stage=session.new.start", "model=gpt-test", "stage=session.new.auth.failed", "token=[REDACTED]"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("log missing %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "must-not-appear") {
		t.Fatalf("log leaked secret:\n%s", text)
	}
	info, err := os.Stat(filepath.Join(stateDirectory, acpDiagnosticLogFilename))
	if err != nil {
		t.Fatalf("Stat error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}
