package cron

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/semistrict/dago/datool"
)

func TestToolsManageOnlyCurrentConversation(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	current := Origin{ConversationID: "current", ChannelID: "telegram"}
	other := Origin{ConversationID: "other", ChannelID: "telegram"}
	schedule, _ := ParseSchedule("in 5m")
	if _, err := store.Create(t.Context(), "other", schedule, other, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	tools := Tools(store, func(context.Context) (Origin, error) { return current, nil })
	if len(tools) != 4 {
		t.Fatalf("tool count = %d", len(tools))
	}
	create, list := tools[0], tools[1]
	createdResult, err := create.Execute(t.Context(), json.RawMessage(`{"prompt":"mine","schedule":"every 5m","name":"heartbeat","repeat_times":2}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	var created JobView
	if err := json.Unmarshal(createdResult.Structured, &created); err != nil {
		t.Fatal(err)
	}
	listedResult, err := list.Execute(t.Context(), json.RawMessage(`{}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	var listed []JobView
	if err := json.Unmarshal(listedResult.Structured, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID || listed[0].Repeat.Times != 2 {
		t.Fatalf("listed = %+v", listed)
	}
	if len(createdResult.Content) != 1 || createdResult.Content[0].Text == "" {
		t.Fatal("tool did not return model-visible JSON")
	}
}

func TestToolsPropagateOriginAndInputFailures(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	want := errors.New("origin unavailable")
	tools := Tools(store, func(context.Context) (Origin, error) { return Origin{}, want })
	if _, err := tools[1].Execute(t.Context(), json.RawMessage(`{}`), datool.Runtime{}); !errors.Is(err, want) {
		t.Fatalf("origin error = %v", err)
	}
	tools = Tools(store, func(context.Context) (Origin, error) { return Origin{ConversationID: "chat"}, nil })
	if _, err := tools[0].Execute(t.Context(), json.RawMessage(`{"prompt":"x","schedule":"tomorrow"}`), datool.Runtime{}); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("schedule error = %v", err)
	}
}

func TestToolsRejectNilRequiredDependencies(t *testing.T) {
	t.Parallel()
	for name, call := range map[string]func(){
		"store":  func() { Tools(nil, func(context.Context) (Origin, error) { return Origin{}, nil }) },
		"origin": func() { Tools(newTestStore(t), nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Tools did not panic")
				}
			}()
			call()
		})
	}
}
