package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestValidateProvenanceRejectsUnknownSource(t *testing.T) {
	err := validateProvenance(
		[]provenanceEntry{testProvenance("missing", "tests/test_contract.py", "test_contract")},
		upstreamManifest{Sources: []upstreamSource{{Name: "upstream", Revision: "abc"}}},
		func(string) string { return "" },
		func(...string) ([]byte, error) { return nil, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "not in the upstream manifest") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateProvenanceRejectsUnsafePath(t *testing.T) {
	err := validateProvenance(
		[]provenanceEntry{testProvenance("upstream", "../tests/test_contract.py", "test_contract")},
		upstreamManifest{Sources: []upstreamSource{{Name: "upstream", Revision: "abc"}}},
		func(string) string { return "" },
		func(...string) ([]byte, error) { return nil, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "clean repository-relative path") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateProvenanceChecksPinnedRevisionAndPath(t *testing.T) {
	var calls [][]string
	git := func(arguments ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), arguments...))
		switch arguments[2] {
		case "rev-parse":
			return []byte("abc\n"), nil
		case "show":
			return []byte("class TestContract:\n    def test_behavior(self):\n        pass\n"), nil
		default:
			return nil, errors.New("unexpected git command")
		}
	}
	err := validateProvenance(
		[]provenanceEntry{testProvenance("upstream", "tests/test_contract.py", "TestContract::test_behavior")},
		upstreamManifest{Sources: []upstreamSource{{Name: "upstream", Revision: "abc", CheckoutEnv: "UPSTREAM_ROOT"}}},
		func(name string) string {
			if name != "UPSTREAM_ROOT" {
				t.Fatalf("environment name = %q", name)
			}
			return "/checkout"
		},
		git,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"-C", "/checkout", "rev-parse", "HEAD"},
		{"-C", "/checkout", "show", "abc:tests/test_contract.py"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestValidateProvenanceRejectsMissingPinnedPath(t *testing.T) {
	git := func(arguments ...string) ([]byte, error) {
		if arguments[2] == "rev-parse" {
			return []byte("abc\n"), nil
		}
		return nil, errors.New("object does not exist")
	}
	err := validateProvenance(
		[]provenanceEntry{testProvenance("upstream", "tests/stale.py", "test_contract")},
		upstreamManifest{Sources: []upstreamSource{{Name: "upstream", Revision: "abc", CheckoutEnv: "UPSTREAM_ROOT"}}},
		func(string) string { return "/checkout" },
		git,
	)
	if err == nil || !strings.Contains(err.Error(), "does not exist at revision") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateProvenanceRejectsWrongCheckoutRevision(t *testing.T) {
	err := validateProvenance(
		[]provenanceEntry{testProvenance("upstream", "tests/test_contract.py", "test_contract")},
		upstreamManifest{Sources: []upstreamSource{{Name: "upstream", Revision: "abc", CheckoutEnv: "UPSTREAM_ROOT"}}},
		func(string) string { return "/checkout" },
		func(...string) ([]byte, error) { return []byte("def\n"), nil },
	)
	if err == nil || !strings.Contains(err.Error(), "want abc") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateProvenanceRejectsInvalidSelector(t *testing.T) {
	err := validateProvenance(
		[]provenanceEntry{testProvenance("upstream", "tests/test_contract.py", "TestContract.test_behavior")},
		upstreamManifest{Sources: []upstreamSource{{Name: "upstream", Revision: "abc"}}},
		func(string) string { return "" },
		func(...string) ([]byte, error) { return nil, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "invalid Python identifier") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateProvenanceRejectsMissingTestSelector(t *testing.T) {
	git := func(arguments ...string) ([]byte, error) {
		if arguments[2] == "rev-parse" {
			return []byte("abc\n"), nil
		}
		return []byte("def test_present():\n    pass\n"), nil
	}
	err := validateProvenance(
		[]provenanceEntry{testProvenance("upstream", "tests/test_contract.py", "test_missing")},
		upstreamManifest{Sources: []upstreamSource{{Name: "upstream", Revision: "abc", CheckoutEnv: "UPSTREAM_ROOT"}}},
		func(string) string { return "/checkout" },
		git,
	)
	if err == nil || !strings.Contains(err.Error(), `test selector "test_missing" does not exist`) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateProvenanceRejectsNestedTestSelector(t *testing.T) {
	git := func(arguments ...string) ([]byte, error) {
		if arguments[2] == "rev-parse" {
			return []byte("abc\n"), nil
		}
		return []byte("class TestContract:\n    def helper(self):\n        def test_nested(self):\n            pass\n"), nil
	}
	err := validateProvenance(
		[]provenanceEntry{testProvenance("upstream", "tests/test_contract.py", "TestContract::test_nested")},
		upstreamManifest{Sources: []upstreamSource{{Name: "upstream", Revision: "abc", CheckoutEnv: "UPSTREAM_ROOT"}}},
		func(string) string { return "/checkout" },
		git,
	)
	if err == nil || !strings.Contains(err.Error(), `test selector "TestContract::test_nested" does not exist`) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateProvenanceRejectsNestedTopLevelTestSelector(t *testing.T) {
	git := func(arguments ...string) ([]byte, error) {
		if arguments[2] == "rev-parse" {
			return []byte("abc\n"), nil
		}
		return []byte("def helper():\n    def test_nested():\n        pass\n"), nil
	}
	err := validateProvenance(
		[]provenanceEntry{testProvenance("upstream", "tests/test_contract.py", "test_nested")},
		upstreamManifest{Sources: []upstreamSource{{Name: "upstream", Revision: "abc", CheckoutEnv: "UPSTREAM_ROOT"}}},
		func(string) string { return "/checkout" },
		git,
	)
	if err == nil || !strings.Contains(err.Error(), `test selector "test_nested" does not exist`) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateProvenanceRequiresTestSelectors(t *testing.T) {
	err := validateProvenance(
		[]provenanceEntry{{Source: "upstream", Path: "tests/test_contract.py"}},
		upstreamManifest{Sources: []upstreamSource{{Name: "upstream", Revision: "abc"}}},
		func(string) string { return "" },
		func(...string) ([]byte, error) { return nil, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "has no test selectors") {
		t.Fatalf("error = %v", err)
	}
}

func testProvenance(source, path string, tests ...string) provenanceEntry {
	return provenanceEntry{Source: source, Path: path, TestSelectors: tests}
}
