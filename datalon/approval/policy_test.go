package approval

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/semistrict/dago/dagent"
)

func TestFromEnvironmentParsesBoundedExactNames(t *testing.T) {
	t.Parallel()
	policy, err := FromEnvironment(map[string]string{
		EnvironmentKey: " local/read, mcp.server/tool, local/read, ,CaseSensitive,mcp.*[x] ",
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CaseSensitive", "local/read", "mcp.*[x]", "mcp.server/tool"}
	if got := policy.ToolNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolNames() = %v, want %v", got, want)
	}
	if policy.Requires("casesensitive") {
		t.Fatal("tool-name matching unexpectedly ignored case")
	}
}

func TestFromEnvironmentRejectsUnsafeOrExcessiveInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		options Options
	}{
		{name: "value bytes", value: "one,two", options: Options{MaxValueBytes: 3}},
		{name: "unique tools", value: "one,two", options: Options{MaxTools: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := FromEnvironment(map[string]string{EnvironmentKey: test.value}, test.options)
			if !errors.Is(err, ErrInvalidEnvironment) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestFromEnvironmentPanicsOnNegativeStaticBoundsBeforeEnvironmentParsing(t *testing.T) {
	t.Parallel()
	tests := []Options{
		{MaxValueBytes: -1},
		{MaxTools: -1},
	}
	for _, options := range tests {
		options := options
		t.Run("negative", func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatal("FromEnvironment did not panic")
				}
			}()
			_, _ = FromEnvironment(map[string]string{EnvironmentKey: strings.Repeat("x", 1<<20)}, options)
		})
	}
}

func TestPolicyMergeForcesConfiguredToolsWithoutMutatingBase(t *testing.T) {
	t.Parallel()
	base := map[string]bool{"local/read": false, "safe": false}
	merged := NewPolicy("local/read", "mcp/tool").MergeConfig(base)
	if merged["local/read"] != true || merged["mcp/tool"] != true || merged["safe"] != false {
		t.Fatalf("merged config = %v", merged)
	}
	if base["local/read"] {
		t.Fatal("MergeConfig mutated base")
	}
	if got := (Policy{}).MergeConfig(nil); got != nil {
		t.Fatalf("empty nil merge = %v", got)
	}
	if got := (Policy{}).MergeConfig(map[string]bool{}); got == nil {
		t.Fatal("non-nil empty base became nil")
	}
}

func TestApprovalRulesPutForcedExactNamesFirst(t *testing.T) {
	t.Parallel()
	base := []dagent.ApprovalRule{{Pattern: "*", When: func(dagent.ToolCallRequest) bool { return false }}}
	rules := NewPolicy("mcp/tool").ApprovalRules(base...)
	if len(rules) != 2 || rules[0].Pattern != "mcp/tool" || rules[1].Pattern != "*" {
		t.Fatalf("rules = %+v", rules)
	}
	matched, err := rules[0].MatchesName("mcp/tool")
	if err != nil || !matched {
		t.Fatalf("forced rule match = %v, %v", matched, err)
	}
	if len(rules[0].AllowedDecisions) != 2 ||
		rules[0].AllowedDecisions[0] != dagent.ApprovalApprove ||
		rules[0].AllowedDecisions[1] != dagent.ApprovalReject {
		t.Fatalf("forced decisions = %v", rules[0].AllowedDecisions)
	}
}

func TestApprovalRulesEscapePatternCharacters(t *testing.T) {
	t.Parallel()
	rules := NewPolicy(`mcp.*[x]\tool`).ApprovalRules()
	for _, candidate := range []struct {
		name string
		want bool
	}{
		{name: `mcp.*[x]\tool`, want: true},
		{name: `mcp.anyx\tool`, want: false},
		{name: `mcp.*x\tool`, want: false},
	} {
		matched, err := rules[0].MatchesName(candidate.name)
		if err != nil || matched != candidate.want {
			t.Fatalf("match %q = %v, %v; want %v", candidate.name, matched, err, candidate.want)
		}
	}
}

func TestNewPolicyPanicsOnInvalidStaticName(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("NewPolicy did not panic")
		}
	}()
	_ = NewPolicy("  ")
}
