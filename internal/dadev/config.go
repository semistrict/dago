package dadev

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type graphSpec struct {
	Path        string
	Description string
}

func (spec *graphSpec) UnmarshalJSON(data []byte) error {
	var path string
	if json.Unmarshal(data, &path) == nil {
		spec.Path = path
		return nil
	}
	var object struct {
		Path        string `json:"path"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return fmt.Errorf("graph must be a path string or object: %w", err)
	}
	spec.Path, spec.Description = object.Path, object.Description
	return nil
}

type projectConfig struct {
	Graphs map[string]graphSpec `json:"graphs"`
	Env    json.RawMessage      `json:"env"`
}

func loadConfig(path string) (projectConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return projectConfig{}, fmt.Errorf("read config: %w", err)
	}
	var config projectConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&config); err != nil {
		return projectConfig{}, fmt.Errorf("decode config: %w", err)
	}
	if len(config.Graphs) == 0 {
		return projectConfig{}, fmt.Errorf("config requires at least one graph")
	}
	for id, graph := range config.Graphs {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(graph.Path) == "" {
			return projectConfig{}, fmt.Errorf("graph IDs and paths must not be empty")
		}
	}
	return config, nil
}

func loadEnvironment(config projectConfig, directory string) ([]string, []string, error) {
	values := map[string]string{}
	watch := []string{}
	if len(config.Env) == 0 || string(config.Env) == "null" {
		path := filepath.Join(directory, ".env")
		if _, err := os.Stat(path); err == nil {
			config.Env, _ = json.Marshal(".env")
		}
	}
	if len(config.Env) != 0 && string(config.Env) != "null" {
		var path string
		if json.Unmarshal(config.Env, &path) == nil {
			path = filepath.Join(directory, filepath.FromSlash(path))
			file, err := os.Open(path)
			if err != nil {
				return nil, nil, fmt.Errorf("open env file: %w", err)
			}
			defer file.Close()
			watch = append(watch, path)
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				key, value, ok := parseEnvLine(scanner.Text())
				if ok {
					values[key] = value
				}
			}
			if err := scanner.Err(); err != nil {
				return nil, nil, fmt.Errorf("read env file: %w", err)
			}
		} else if err := json.Unmarshal(config.Env, &values); err != nil {
			return nil, nil, fmt.Errorf("env must be a file path or string map: %w", err)
		}
	}
	merged := map[string]string{}
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			merged[key] = value
		}
	}
	for key, value := range values {
		merged[key] = value
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+merged[key])
	}
	return result, watch, nil
}

func parseEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "export ")
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	key, value, ok := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if !ok || key == "" {
		return "", "", false
	}
	if len(value) >= 2 && (value[0] == '\'' && value[len(value)-1] == '\'' || value[0] == '"' && value[len(value)-1] == '"') {
		value = value[1 : len(value)-1]
	}
	return key, value, true
}
