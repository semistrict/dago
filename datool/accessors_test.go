package datool

import "testing"

type readerMap map[string]any

func (values readerMap) Get(key string) (any, bool) {
	value, ok := values[key]
	return value, ok
}

type runtimeRecord struct {
	Name string `json:"name"`
}

func TestTypedRuntimeAccessors(t *testing.T) {
	state := readerMap{"live": runtimeRecord{Name: "live"}, "restored": map[string]any{"name": "restored"}}
	if got, ok := StateAs[runtimeRecord](state, "live"); !ok || got.Name != "live" {
		t.Fatalf("StateAs(live) = %#v, %v", got, ok)
	}
	if got, ok := StateAs[runtimeRecord](state, "restored"); !ok || got.Name != "restored" {
		t.Fatalf("StateAs(restored) = %#v, %v", got, ok)
	}
	if got, ok := ResumeAs[runtimeRecord](Runtime{Resume: state["restored"]}); !ok || got.Name != "restored" {
		t.Fatalf("ResumeAs() = %#v, %v", got, ok)
	}
	if got, ok := InterruptAs[runtimeRecord](Interrupt{Value: state["restored"]}); !ok || got.Name != "restored" {
		t.Fatalf("InterruptAs() = %#v, %v", got, ok)
	}
	deps := &runtimeRecord{Name: "deps"}
	if got, ok := DepsAs[*runtimeRecord](Runtime{Deps: deps}); !ok || got != deps {
		t.Fatalf("DepsAs() = %#v, %v", got, ok)
	}
}
