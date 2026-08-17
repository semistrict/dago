package optionvalue

import "testing"

type testOptions struct{ value string }

func TestResolve(t *testing.T) {
	if got := Resolve[testOptions]("test", nil); got != (testOptions{}) {
		t.Fatalf("omitted options = %#v", got)
	}
	want := testOptions{value: "configured"}
	if got := Resolve("test", []testOptions{want}); got != want {
		t.Fatalf("resolved options = %#v", got)
	}
	t.Run("multiple", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("multiple options did not panic")
			}
		}()
		Resolve("test", []testOptions{{}, {}})
	})
}
