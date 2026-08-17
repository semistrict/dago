// Package optionvalue resolves optional zero-or-one configuration values.
package optionvalue

// Resolve returns the supplied value or its zero value when omitted.
// Supplying multiple configuration structs is a programmer mistake.
func Resolve[T any](owner string, values []T) T {
	if len(values) > 1 {
		panic(owner + " accepts at most one options value")
	}
	if len(values) == 1 {
		return values[0]
	}
	var zero T
	return zero
}
