// Package approval provides Talon's experimental channel tool-approval policy.
// It is a convenience layer, not a production authorization, sandbox, channel
// administration, or multi-tenant security boundary.
package approval

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/semistrict/dago/dagent"
)

const EnvironmentKey = "DEEPAGENTS_TALON_INTERRUPT_ON_TOOLS"

var (
	// ErrInvalidEnvironment reports an unsafe or excessive approval overlay.
	ErrInvalidEnvironment = errors.New("invalid Talon tool approval environment")
)

// Options bounds external approval configuration. Its zero value uses
// DefaultOptions and cannot disable limits accidentally.
type Options struct {
	MaxValueBytes int
	MaxTools      int
}

// DefaultOptions returns useful finite environment parsing limits.
func DefaultOptions() Options {
	return Options{MaxValueBytes: 64 << 10, MaxTools: 1024}
}

func (options Options) withDefaults() Options {
	defaults := DefaultOptions()
	if options.MaxValueBytes == 0 {
		options.MaxValueBytes = defaults.MaxValueBytes
	}
	if options.MaxTools == 0 {
		options.MaxTools = defaults.MaxTools
	}
	return options
}

func (options Options) validateStaticBounds() {
	switch {
	case options.MaxValueBytes < 0:
		panic("datalon approval: negative maximum environment value size")
	case options.MaxTools < 0:
		panic("datalon approval: negative maximum tool count")
	}
}

// Policy is an immutable set of exact tool names that must be approved. Its
// zero value requires no additional approvals.
type Policy struct{ tools map[string]struct{} }

// NewPolicy constructs a static exact-name policy. Blank static names are
// programmer errors and panic; unknown names are valid so dynamically loaded
// MCP tools can be configured before discovery.
func NewPolicy(toolNames ...string) Policy {
	for _, name := range toolNames {
		if strings.TrimSpace(name) == "" {
			panic("datalon approval: blank static tool name")
		}
	}
	policy, err := policyFromNames(toolNames, len(toolNames))
	if err != nil {
		panic(err)
	}
	return policy
}

// FromEnvironment parses EnvironmentKey. A nil map reads the process
// environment. Comma-separated names are trimmed, empty entries are ignored,
// case is preserved, and duplicates collapse.
func FromEnvironment(environment map[string]string, options Options) (Policy, error) {
	options.validateStaticBounds()
	options = options.withDefaults()
	if options.MaxValueBytes <= 0 || options.MaxTools <= 0 {
		return Policy{}, fmt.Errorf("%w: limits must be positive", ErrInvalidEnvironment)
	}
	raw := ""
	if environment == nil {
		raw = os.Getenv(EnvironmentKey)
	} else {
		raw = environment[EnvironmentKey]
	}
	if len(raw) > options.MaxValueBytes {
		return Policy{}, fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidEnvironment, EnvironmentKey, options.MaxValueBytes)
	}
	parts := strings.Split(raw, ",")
	names := make([]string, 0, min(len(parts), options.MaxTools))
	for _, part := range parts {
		if name := strings.TrimSpace(part); name != "" {
			names = append(names, name)
		}
	}
	return policyFromNames(names, options.MaxTools)
}

func policyFromNames(names []string, maximum int) (Policy, error) {
	tools := make(map[string]struct{}, min(len(names), maximum))
	for _, name := range names {
		tools[name] = struct{}{}
		if len(tools) > maximum {
			return Policy{}, fmt.Errorf("%w: more than %d unique tools", ErrInvalidEnvironment, maximum)
		}
	}
	if len(tools) == 0 {
		return Policy{}, nil
	}
	return Policy{tools: tools}, nil
}

// Requires reports whether the exact tool name is forced through channel approval.
func (policy Policy) Requires(toolName string) bool {
	_, required := policy.tools[toolName]
	return required
}

// ToolNames returns the forced names in deterministic order.
func (policy Policy) ToolNames() []string {
	result := make([]string, 0, len(policy.tools))
	for name := range policy.tools {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// Merge returns the union of base and policy without mutating either input.
func (policy Policy) Merge(base Policy) Policy {
	if len(base.tools) == 0 && len(policy.tools) == 0 {
		return Policy{}
	}
	merged := make(map[string]struct{}, len(base.tools)+len(policy.tools))
	for name := range base.tools {
		merged[name] = struct{}{}
	}
	for name := range policy.tools {
		merged[name] = struct{}{}
	}
	return Policy{tools: merged}
}

// MergeConfig copies base and forces every policy name to true. A configured
// name therefore overrides a same-name false value, matching the pinned overlay.
func (policy Policy) MergeConfig(base map[string]bool) map[string]bool {
	if base == nil && len(policy.tools) == 0 {
		return nil
	}
	merged := make(map[string]bool, len(base)+len(policy.tools))
	for name, enabled := range base {
		merged[name] = enabled
	}
	for name := range policy.tools {
		merged[name] = true
	}
	return merged
}

// ApprovalRules prepends exact forced rules to base so application-provided
// false/conditional behavior cannot disable the environment overlay. The
// returned slice is independent of base and works for local and MCP tool names.
func (policy Policy) ApprovalRules(base ...dagent.ApprovalRule) []dagent.ApprovalRule {
	names := policy.ToolNames()
	rules := make([]dagent.ApprovalRule, 0, len(names)+len(base))
	for _, name := range names {
		rules = append(rules, dagent.ApprovalRule{
			Pattern: exactApprovalPattern(name), Description: "Talon channel approval override",
			AllowedDecisions: []dagent.ApprovalDecision{dagent.ApprovalApprove, dagent.ApprovalReject},
		})
	}
	return append(rules, base...)
}

func exactApprovalPattern(name string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)
	return replacer.Replace(name)
}
