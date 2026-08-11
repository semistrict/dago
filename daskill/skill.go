// Package daskill implements the language-neutral Agent Skills metadata contract.
package daskill

import (
	"fmt"
	"os"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	MaxNameLength          = 64
	MaxDescriptionLength   = 1024
	MaxCompatibilityLength = 500
)

// Skill is the progressively disclosed metadata for one SKILL.md file.
type Skill struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Path          string            `json:"path"`
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	AllowedTools  []string          `json:"allowed_tools,omitempty"`
	Body          string            `json:"body,omitempty"`
}

// ValidationError identifies invalid Agent Skills metadata.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// ParseContent parses SKILL.md content for middleware discovery. Structural
// failures are errors; specification mismatches that do not prevent progressive
// disclosure are returned as warnings, matching Deep Agents behavior.
func ParseContent(content, filePath string) (Skill, []string, error) {
	filePath = strings.ReplaceAll(filePath, `\`, "/")
	parsed, err := decode(content, filePath)
	if err != nil {
		return Skill{}, nil, err
	}
	var warnings []string
	directory := path.Base(path.Dir(filePath))
	if reason := NameViolation(parsed.Name, directory); reason != "" {
		warnings = append(warnings, fmt.Sprintf("skill %q in %s does not follow the skill specification: %s", parsed.Name, filePath, reason))
	}
	if len([]rune(parsed.Description)) > MaxDescriptionLength {
		warnings = append(warnings, fmt.Sprintf("skill description in %s exceeds %d characters and was truncated", filePath, MaxDescriptionLength))
		parsed.Description = string([]rune(parsed.Description)[:MaxDescriptionLength])
	}
	if len([]rune(parsed.Compatibility)) > MaxCompatibilityLength {
		warnings = append(warnings, fmt.Sprintf("skill compatibility in %s exceeds %d characters and was truncated", filePath, MaxCompatibilityLength))
		parsed.Compatibility = string([]rune(parsed.Compatibility)[:MaxCompatibilityLength])
	}
	return parsed, warnings, nil
}

// ParseFile parses a local SKILL.md for product-facing management commands.
// Unlike middleware discovery, malformed specification fields are rejected.
func ParseFile(filePath string) (Skill, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return Skill{}, err
	}
	parsed, err := decode(string(content), filePath)
	if err != nil {
		return Skill{}, err
	}
	if reason := NameViolation(parsed.Name, ""); reason != "" {
		return Skill{}, &ValidationError{Message: reason}
	}
	if len([]rune(parsed.Description)) > MaxDescriptionLength {
		return Skill{}, &ValidationError{Message: "description exceeds maximum length"}
	}
	if len([]rune(parsed.Compatibility)) > MaxCompatibilityLength {
		return Skill{}, &ValidationError{Message: "compatibility exceeds maximum length"}
	}
	return parsed, nil
}

// ValidateName validates the name portion of the Agent Skills specification.
func ValidateName(name string) error {
	if reason := NameViolation(name, ""); reason != "" {
		return &ValidationError{Message: reason}
	}
	return nil
}

// NameViolation returns a human-readable specification violation, or empty.
// When directory is non-empty, the skill name must match its parent directory.
func NameViolation(name, directory string) string {
	if len([]rune(name)) == 0 || len([]rune(name)) > MaxNameLength {
		return "name must be 1-64 characters"
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return "name cannot start or end with hyphen"
	}
	if strings.Contains(name, "--") {
		return "name cannot contain consecutive hyphens"
	}
	for _, value := range name {
		if value == '-' || unicode.IsDigit(value) || unicode.IsLetter(value) && unicode.IsLower(value) {
			continue
		}
		return "name can only contain lowercase letters, digits, and hyphens"
	}
	if directory != "" && name != directory {
		return fmt.Sprintf("name must match directory %q", directory)
	}
	return ""
}

// ExtractBody returns the markdown after a valid frontmatter envelope.
func ExtractBody(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			return strings.TrimSpace(strings.Join(lines[index+1:], "\n"))
		}
	}
	return ""
}

func decode(content, filePath string) (Skill, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	if !utf8.ValidString(content) || len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return Skill{}, &ValidationError{Message: "SKILL.md must start with valid YAML frontmatter (---)"}
	}
	end := -1
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			end = index
			break
		}
	}
	if end < 0 {
		return Skill{}, &ValidationError{Message: "SKILL.md frontmatter not properly closed with ---"}
	}
	var data map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &data); err != nil {
		return Skill{}, &ValidationError{Message: "invalid YAML frontmatter: " + err.Error()}
	}
	if data == nil {
		return Skill{}, &ValidationError{Message: "SKILL.md frontmatter must be a mapping"}
	}
	stringValue := func(key string) string {
		value, exists := data[key]
		if !exists || value == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(value))
	}
	parsed := Skill{
		Name:          stringValue("name"),
		Description:   stringValue("description"),
		Path:          filePath,
		License:       stringValue("license"),
		Compatibility: stringValue("compatibility"),
		Metadata:      parseMetadata(data["metadata"]),
		AllowedTools:  parseAllowedTools(data["allowed-tools"]),
		Body:          strings.TrimSpace(strings.Join(lines[end+1:], "\n")),
	}
	if parsed.Name == "" || parsed.Description == "" {
		return Skill{}, &ValidationError{Message: "name and description are required"}
	}
	return parsed, nil
}

func parseAllowedTools(value any) []string {
	switch typed := value.(type) {
	case string:
		return strings.FieldsFunc(typed, func(r rune) bool { return unicode.IsSpace(r) || r == ',' })
	case []any:
		var result []string
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				result = append(result, strings.TrimSpace(text))
			}
		}
		return result
	default:
		return nil
	}
}

func parseMetadata(value any) map[string]string {
	values, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, item := range values {
		result[key] = fmt.Sprint(item)
	}
	return result
}
