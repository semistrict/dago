package dago

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

// These tests port the applicable checkpoint contracts from the pinned
// test_snapshot and test_snapshot_persistence suites. The local state field
// materializes WAFL dirty-page records instead of Python bsdiff records.

func TestInterpreterSnapshotChainRoundTripAndAssociativity(t *testing.T) {
	middleware, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{}}, "chain")
	field := middleware.Fields[interpreterSnapshotKey]
	state := field.Initial()
	records := make([]any, 0, 3)
	for _, code := range []string{
		`const anchor = 40; anchor`,
		`const offset = 2; anchor + offset`,
		`globalThis.extra = "kept"; [anchor + offset, extra].join(":")`,
	} {
		result, err := executeInterpreterTest(t, eval, "chain", code, dastate.Values{interpreterSnapshotKey: state})
		if err != nil {
			t.Fatal(err)
		}
		record := result.Update[interpreterSnapshotKey]
		records = append(records, record)
		state, err = field.Reduce(state, []any{record})
		if err != nil {
			t.Fatal(err)
		}
	}

	whole, err := field.Reduce(field.Initial(), records)
	if err != nil {
		t.Fatal(err)
	}
	for split := 0; split <= len(records); split++ {
		left, err := field.Reduce(field.Initial(), records[:split])
		if err != nil {
			t.Fatal(err)
		}
		combined, err := field.Reduce(left, records[split:])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(materializedInterpreterSnapshot(combined), materializedInterpreterSnapshot(whole)) {
			t.Fatalf("split %d reconstructed different snapshot bytes", split)
		}
	}

	restored, err := executeInterpreterTest(t, eval, "chain", `[anchor + offset, extra].join(":")`, dastate.Values{interpreterSnapshotKey: whole})
	if err != nil || !strings.Contains(restored.Content[0].Text, "42:kept") {
		t.Fatalf("restored content = %q, error = %v", firstInterpreterText(restored), err)
	}
}

func TestInterpreterSnapshotReducerClearAnchorUnknownAndClone(t *testing.T) {
	middleware, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{}}, "records")
	field := middleware.Fields[interpreterSnapshotKey]
	first, err := executeInterpreterTest(t, eval, "records", `const oldValue = 1; oldValue`, dastate.Values{})
	if err != nil {
		t.Fatal(err)
	}
	firstRecord := first.Update[interpreterSnapshotKey]
	firstState, err := field.Reduce(field.Initial(), []any{firstRecord})
	if err != nil {
		t.Fatal(err)
	}
	second, err := executeInterpreterTest(t, eval, "records-new", `const newValue = 9; newValue`, dastate.Values{})
	if err != nil {
		t.Fatal(err)
	}
	secondRecord := second.Update[interpreterSnapshotKey]

	ignored, err := field.Reduce(firstState, []any{42, map[string]any{"kind": "unknown", "data": []byte("noise")}})
	if err != nil || !bytes.Equal(materializedInterpreterSnapshot(ignored), materializedInterpreterSnapshot(firstState)) {
		t.Fatalf("unknown records changed state: %v", err)
	}
	cleared, err := field.Reduce(firstState, []any{map[string]any{"kind": "clear", "data": []byte(nil)}})
	if err != nil || len(materializedInterpreterSnapshot(cleared)) != 0 {
		t.Fatalf("clear state bytes = %d, error = %v", len(materializedInterpreterSnapshot(cleared)), err)
	}
	reanchored, err := field.Reduce(firstState, []any{secondRecord})
	if err != nil || !bytes.Equal(materializedInterpreterSnapshot(reanchored), secondRecord.(map[string]any)["data"].([]byte)) {
		t.Fatalf("anchor did not replace prior state: %v", err)
	}
	result, err := executeInterpreterTest(t, eval, "records-new", `typeof oldValue + ":" + newValue`, dastate.Values{interpreterSnapshotKey: reanchored})
	if err != nil || !strings.Contains(result.Content[0].Text, "undefined:9") {
		t.Fatalf("reanchored content = %q, error = %v", firstInterpreterText(result), err)
	}

	clone := field.Clone(firstState)
	cloneBytes := clone.(map[string]any)["snapshot"].([]byte)
	originalBytes := firstState.(map[string]any)["snapshot"].([]byte)
	originalFirst := originalBytes[0]
	cloneBytes[0] ^= 0xff
	if originalBytes[0] != originalFirst {
		t.Fatal("snapshot clone aliases the original bytes")
	}
}

func TestInterpreterInvalidOrOversizedSnapshotsStartFresh(t *testing.T) {
	_, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{}}, "invalid")
	invalid := map[string]any{"snapshot": []byte("not-a-snapshot")}
	result, err := executeInterpreterTest(t, eval, "invalid", `typeof missing`, dastate.Values{interpreterSnapshotKey: invalid})
	if err != nil || !strings.Contains(result.Content[0].Text, "undefined") {
		t.Fatalf("invalid snapshot content = %q, error = %v", firstInterpreterText(result), err)
	}
	if result.Update[interpreterSnapshotKey].(map[string]any)["kind"] != "snap" {
		t.Fatalf("invalid snapshot did not produce a fresh anchor")
	}

	_, cappedEval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{}, MaxSnapshotBytes: 64}, "capped")
	capped, err := executeInterpreterTest(t, cappedEval, "capped", `globalThis.value = 42`, dastate.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if capped.Update[interpreterSnapshotKey].(map[string]any)["kind"] != "clear" {
		t.Fatalf("oversized snapshot update kind = %v", capped.Update[interpreterSnapshotKey].(map[string]any)["kind"])
	}
}

func TestInterpreterSnapshotPersistsBindingsAndTopLevelAwaitAcrossTurns(t *testing.T) {
	middleware, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{}}, "bindings")
	state := any(nil)
	for _, code := range []string{
		`const base = await Promise.resolve(40); let delta = 1; base + delta`,
		`delta += 1; base + delta`,
		`const answer = await Promise.resolve(base + delta); answer`,
	} {
		values := dastate.Values{}
		if state != nil {
			values[interpreterSnapshotKey] = state
		}
		result, err := executeInterpreterTest(t, eval, "bindings", code, values)
		if err != nil || result.Status == damessage.ToolStatusError {
			t.Fatalf("code %q content = %q, error = %v", code, firstInterpreterText(result), err)
		}
		state = reduceInterpreterTest(t, middleware, state, result)
	}
	final, err := executeInterpreterTest(t, eval, "bindings", `[base, delta, answer].join(":")`, dastate.Values{interpreterSnapshotKey: state})
	if err != nil || !strings.Contains(final.Content[0].Text, "40:2:42") {
		t.Fatalf("final content = %q, error = %v", firstInterpreterText(final), err)
	}
}

func TestInterpreterSnapshotsIsolateThreads(t *testing.T) {
	middleware, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{}}, "thread-a")
	a, err := executeInterpreterTest(t, eval, "thread-a", `const marker = "a"; marker`, dastate.Values{})
	if err != nil {
		t.Fatal(err)
	}
	aState := reduceInterpreterTest(t, middleware, nil, a)
	b, err := executeInterpreterTest(t, eval, "thread-b", `const marker = "b"; marker`, dastate.Values{})
	if err != nil {
		t.Fatal(err)
	}
	bState := reduceInterpreterTest(t, middleware, nil, b)
	for _, test := range []struct {
		thread string
		state  any
		want   string
	}{{"thread-a", aState, ">a</result>"}, {"thread-b", bState, ">b</result>"}} {
		result, err := executeInterpreterTest(t, eval, test.thread, `marker`, dastate.Values{interpreterSnapshotKey: test.state})
		if err != nil || !strings.Contains(result.Content[0].Text, test.want) {
			t.Fatalf("thread %s content = %q, error = %v", test.thread, firstInterpreterText(result), err)
		}
	}
}

func TestInterpreterRunsDifferentThreadsConcurrently(t *testing.T) {
	_, eval, _ := prepareInterpreterTest(t, Interpreter{PTC: []string{}}, "parallel-0")
	const count = 8
	errorsByThread := make([]error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			thread := fmt.Sprintf("parallel-%d", index)
			result, err := executeInterpreterTest(t, eval, thread, fmt.Sprintf("%d * 10", index), dastate.Values{})
			if err == nil && !strings.Contains(firstInterpreterText(result), fmt.Sprintf(">%d</result>", index*10)) {
				err = fmt.Errorf("content = %q", firstInterpreterText(result))
			}
			errorsByThread[index] = err
		}()
	}
	wait.Wait()
	for index, err := range errorsByThread {
		if err != nil {
			t.Errorf("thread %d: %v", index, err)
		}
	}
}

func firstInterpreterText(result datool.Result) string {
	if len(result.Content) == 0 {
		return ""
	}
	return result.Content[0].Text
}
