package dacode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const acpDiagnosticLogFilename = "dacode-acp.log"

type acpDiagnosticLog struct {
	mu    sync.Mutex
	file  *os.File
	clock func() time.Time
}

func openACPDiagnosticLog(stateDirectory string) (*acpDiagnosticLog, error) {
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	path := filepath.Join(stateDirectory, acpDiagnosticLogFilename)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", acpDiagnosticLogFilename, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure %s: %w", acpDiagnosticLogFilename, err)
	}
	return &acpDiagnosticLog{file: file, clock: time.Now}, nil
}

func (log *acpDiagnosticLog) Event(stage string, details ...string) {
	if log == nil {
		return
	}
	cleanStage := sanitizeLocalDevDiagnostic(stage, 128)
	cleanDetails := make([]string, 0, len(details))
	for _, detail := range details {
		if clean := sanitizeLocalDevDiagnostic(detail, 512); clean != "" {
			cleanDetails = append(cleanDetails, clean)
		}
	}
	line := log.clock().UTC().Format(time.RFC3339Nano) + " stage=" + cleanStage
	if len(cleanDetails) != 0 {
		line += " detail=" + fmt.Sprintf("%q", strings.Join(cleanDetails, "; "))
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.file == nil {
		return
	}
	_, _ = fmt.Fprintln(log.file, line)
	_ = log.file.Sync()
}

func (log *acpDiagnosticLog) Failure(stage string, err error) {
	if err == nil {
		log.Event(stage)
		return
	}
	log.Event(stage, err.Error())
}

func (log *acpDiagnosticLog) Close() error {
	if log == nil {
		return nil
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.file == nil {
		return nil
	}
	err := log.file.Close()
	log.file = nil
	return err
}
