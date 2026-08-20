package dagent

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
)

type depsCheckingChat struct {
	profile damodel.Profile
}

func (chat depsCheckingChat) Profile() damodel.Profile { return chat.profile }

func (depsCheckingChat) Invoke(context.Context, damodel.Request) (damodel.Response, error) {
	return damodel.Response{Message: damessage.Assistant("ok")}, nil
}

func (depsCheckingChat) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return nil, fmt.Errorf("stream not supported")
}

func TestInvocationDepsAreDistinctAcrossConcurrentCalls(t *testing.T) {
	seen := make(chan string, 2)
	middleware := Middleware{Name: "deps_guard", BeforeModel: func(_ context.Context, _ dastate.Values, runtime Runtime) (dastate.Values, error) {
		value, _ := runtime.Deps.(string)
		seen <- value
		return nil, nil
	}}
	agent := New(depsCheckingChat{}, Options{Deps: "compiled", Middleware: []Middleware{middleware}})
	var wait sync.WaitGroup
	errors := make(chan error, 2)
	for _, value := range []string{"alpha", "beta"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := agent.Invoke(t.Context(), Prompt("go"), WithDeps(value))
			errors <- err
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	values := map[string]bool{<-seen: true, <-seen: true}
	if !values["alpha"] || !values["beta"] || len(values) != 2 {
		t.Fatalf("invocation dependencies = %#v", values)
	}
}

func TestInvocationDepsAreResuppliedWhenResuming(t *testing.T) {
	saver := dacheckpoint.NewMemorySaver()
	seen := []string{}
	middleware := Middleware{Name: "resume_deps_guard", BeforeAgent: func(_ context.Context, _ dastate.Values, runtime Runtime) (dastate.Values, error) {
		value, _ := runtime.Deps.(string)
		seen = append(seen, value)
		return nil, nil
	}}
	agent := New(depsCheckingChat{}, Options{Deps: "compiled", Middleware: []Middleware{middleware}, Saver: saver})
	config := dacheckpoint.Config{ThreadID: "context-resume"}
	if _, err := agent.Invoke(t.Context(), FromCheckpoint(config), Prompt("first"), WithDeps("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Invoke(t.Context(), FromCheckpoint(config), Prompt("second"), WithDeps("second")); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "first" || seen[1] != "second" {
		t.Fatalf("resumed invocation dependencies = %#v", seen)
	}
}
