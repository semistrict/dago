// Package state defines language-neutral graph state values and update markers.
package state

// Values is a graph state bag. Built-in APIs register reducers and typed helpers for
// known keys while allowing applications to carry additional values deliberately.
type Values map[string]any

// Get implements the read-only state interface injected into tools.
func (values Values) Get(key string) (any, bool) {
	value, ok := values[key]
	return value, ok
}

// Clone returns a shallow copy. Reducer and checkpoint boundaries perform field-aware
// deep copies through their registered cloners.
func (values Values) Clone() Values {
	if values == nil {
		return nil
	}
	copy := make(Values, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

// Overwrite resets a reducer-backed field instead of combining with other writes in
// the same superstep.
type Overwrite struct {
	Value any `json:"value"`
}

// Batch preserves several independent writes to one reducer-backed field in a
// single graph command. The graph expands the values before invoking the field
// reducer, so batching does not change reducer semantics.
type Batch struct {
	Values []any
}
