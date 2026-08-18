package dacode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const displaySettingsFilename = "display.json"

const maxDisplaySettingsBytes = 4 << 10

type displaySettings struct {
	ShowMessageTimestamps bool   `json:"show_message_timestamps"`
	ShowScrollbar         bool   `json:"show_scrollbar"`
	ShowDiffLineNumbers   bool   `json:"show_diff_line_numbers"`
	ThreadRelativeTime    bool   `json:"thread_relative_time"`
	ThreadAgent           string `json:"thread_agent,omitempty"`
	ThreadAllAgents       bool   `json:"thread_all_agents"`
}

type displaySettingName string

const (
	displayMessageTimestamps displaySettingName = "timestamps"
	displayChatScrollbar     displaySettingName = "scrollbar"
	displayDiffLineNumbers   displaySettingName = "line-numbers"
)

func toggleDisplaySetting(settings displaySettings, name displaySettingName) (displaySettings, bool) {
	switch name {
	case displayMessageTimestamps:
		settings.ShowMessageTimestamps = !settings.ShowMessageTimestamps
	case displayChatScrollbar:
		settings.ShowScrollbar = !settings.ShowScrollbar
	case displayDiffLineNumbers:
		settings.ShowDiffLineNumbers = !settings.ShowDiffLineNumbers
	default:
		return settings, false
	}
	return settings, true
}

func loadDisplaySettings(path string) (displaySettings, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultDisplaySettings(), nil
	}
	if err != nil {
		return displaySettings{}, fmt.Errorf("read display settings: %w", err)
	}
	defer file.Close()
	var data bytes.Buffer
	if _, err := data.ReadFrom(io.LimitReader(file, maxDisplaySettingsBytes+1)); err != nil {
		return displaySettings{}, fmt.Errorf("read display settings: %w", err)
	}
	if data.Len() > maxDisplaySettingsBytes {
		return displaySettings{}, fmt.Errorf("read display settings: file exceeds %d bytes", maxDisplaySettingsBytes)
	}
	settings := defaultDisplaySettings()
	if err := json.Unmarshal(data.Bytes(), &settings); err != nil {
		return displaySettings{}, fmt.Errorf("decode display settings: %w", err)
	}
	return settings, nil
}

func defaultDisplaySettings() displaySettings {
	return displaySettings{ShowDiffLineNumbers: true, ThreadRelativeTime: true, ThreadAllAgents: true}
}

func saveDisplaySettings(path string, settings displaySettings) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create display settings directory: %w", err)
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode display settings: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".display-*.json")
	if err != nil {
		return fmt.Errorf("create temporary display settings: %w", err)
	}
	temporaryPath := temporary.Name()
	defer temporary.Close()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure display settings: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write display settings: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync display settings: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close display settings: %w", err)
	}
	if err := replaceFileDurably(temporaryPath, path); err != nil {
		return fmt.Errorf("replace display settings: %w", err)
	}
	return nil
}
