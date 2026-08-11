package dacheckpoint

import (
	"regexp"
	"strings"
	"testing"
)

func TestNewIDIsUUIDv6AndLexicallyIncreasing(t *testing.T) {
	first, err := NewID(1)
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	second, err := NewID(2)
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-6[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !pattern.MatchString(first) || !pattern.MatchString(second) {
		t.Fatalf("ids are not UUIDv6: %q, %q", first, second)
	}
	if second <= first {
		t.Fatalf("second id %q is not greater than first %q", second, first)
	}
}

func TestNextVersionUsesLexicalPythonShape(t *testing.T) {
	first, err := NextVersion("")
	if err != nil {
		t.Fatalf("NextVersion() error = %v", err)
	}
	second, err := NextVersion(first)
	if err != nil {
		t.Fatalf("NextVersion() error = %v", err)
	}
	if !strings.HasPrefix(first, "00000000000000000000000000000001.") {
		t.Fatalf("first version = %q", first)
	}
	if !strings.HasPrefix(second, "00000000000000000000000000000002.") {
		t.Fatalf("second version = %q", second)
	}
	if second <= first {
		t.Fatalf("second version %q is not greater than first %q", second, first)
	}
}

func TestNextVersionRejectsInvalidCurrentValue(t *testing.T) {
	if _, err := NextVersion("not-a-version"); err == nil {
		t.Fatal("NextVersion() error = nil, want non-nil")
	}
}
