package dahousekeeping

import (
	"context"
	"runtime/debug"
	"testing"
)

func TestDependencyFloorCheckerUsesExecutedReplacementAndReportsStale(t *testing.T) {
	info := debug.BuildInfo{
		Main: debug.Module{Path: "example/main", Version: "v1.2.0"},
		Deps: []*debug.Module{
			{Path: "example/fresh", Version: "v1.4.0"},
			{Path: "example/stale", Version: "v1.1.0"},
			{Path: "example/replaced", Version: "v9.0.0", Replace: &debug.Module{Path: "example/fork", Version: "v1.0.0"}},
			{Path: "example/local", Version: "v1.9.0", Replace: &debug.Module{Path: "../local"}},
		},
	}
	checker := NewDependencyFloorChecker(info, []DependencyFloor{
		{Module: "example/main", Minimum: "v1.2.0"},
		{Module: "example/fresh", Minimum: "v1.3.0"},
		{Module: "example/stale", Minimum: "v1.2.0"},
		{Module: "example/replaced", Minimum: "v1.1.0"},
		{Module: "example/local", Minimum: "v1.0.0"},
		{Module: "example/missing", Minimum: "v1.0.0"},
	})
	report, err := checker.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Version != 1 || report.Stale != 2 || report.Unresolved != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	want := []DependencyStatus{
		DependencySatisfied, DependencySatisfied, DependencyStale,
		DependencyStale, DependencyUnavailable, DependencyUnavailable,
	}
	for index, status := range want {
		if report.Results[index].Status != status {
			t.Fatalf("result %d: got %q, want %q", index, report.Results[index].Status, status)
		}
	}
	if report.Results[4].Installed != "" {
		t.Fatalf("local replacement path leaked into report: %+v", report.Results[4])
	}
}

func TestDependencyFloorCheckerSemverOrdering(t *testing.T) {
	info := debug.BuildInfo{Deps: []*debug.Module{
		{Path: "example/prerelease", Version: "v1.2.0-rc.10"},
		{Path: "example/pseudo", Version: "v0.0.0-20260812112233-abcdef123456"},
		{Path: "example/large", Version: "v1.0.0-999999999999999999999999999999"},
	}}
	report, err := NewDependencyFloorChecker(info, []DependencyFloor{
		{Module: "example/prerelease", Minimum: "v1.2.0-rc.2"},
		{Module: "example/pseudo", Minimum: "v0.0.0-20260811112233-abcdef123456"},
		{Module: "example/large", Minimum: "v1.0.0-100000000000000000000000000000"},
	}).Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Stale != 0 || report.Unresolved != 0 {
		t.Fatalf("semantic versions were compared incorrectly: %+v", report)
	}
}

func TestDependencyFloorCheckerCancellationAndValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := NewDependencyFloorChecker(debug.BuildInfo{}, []DependencyFloor{{Module: "example/module", Minimum: "v1.0.0"}}).Check(ctx)
	if err != context.Canceled {
		t.Fatalf("got %v, want context cancellation", err)
	}
	assertPanics(t, func() {
		NewDependencyFloorChecker(debug.BuildInfo{}, []DependencyFloor{{Module: "example/module", Minimum: "1.0"}})
	})
	assertPanics(t, func() {
		NewDependencyFloorChecker(debug.BuildInfo{}, []DependencyFloor{
			{Module: "example/module", Minimum: "v1.0.0"},
			{Module: "example/module", Minimum: "v2.0.0"},
		})
	})
}
