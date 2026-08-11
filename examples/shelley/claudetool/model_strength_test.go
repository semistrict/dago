package claudetool

import "strings"

// isStrongModel is retained only for the copied legacy schema-selection test.
// Production editing schemas are selected by dago rather than model-name tests.
func isStrongModel(modelID string) bool {
	lower := strings.ToLower(modelID)
	return strings.Contains(lower, "gpt-5.6-sol") || strings.Contains(lower, "gpt-5.6-terra") || strings.Contains(lower, "gpt-5.6-luna")
}
