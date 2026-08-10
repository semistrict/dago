package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
	"github.com/semistrict/dago/state"
)

func TestNemotronEntityGuardKeepsCurrentBranchBound(t *testing.T) {
	history := []message.Message{message.Human("What service is affected by the current incident?")}
	current := message.ToolCall{ID: "current", Name: "get_current_incident_id", Arguments: json.RawMessage(`{}`)}
	currentRequest := message.Assistant("")
	currentRequest.ToolCalls = []message.ToolCall{current}
	history = append(history, currentRequest, message.Tool(current.ID, "41017"))
	relation := message.ToolCall{ID: "relation", Name: "get_incident_service", Arguments: json.RawMessage(`{"incident_id":41017}`)}
	relationRequest := message.Assistant("")
	relationRequest.ToolCalls = []message.ToolCall{relation}
	history = append(history, relationRequest, message.Tool(relation.ID, "8514"))

	script := modeltest.New(model.Profile{},
		modeltest.Step{Response: model.Response{Message: message.Assistant("The current incident affects service 8514.")}},
		modeltest.Step{Check: func(request model.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Name != nemotronEntitySource || !strings.Contains(last.TextContent(), "service_id 8514") || !strings.Contains(last.TextContent(), "get_service_title") {
				return errors.New("entity resolution nudge missing")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("The current incident affects checkout-web.")}},
	)
	compiled, err := agent.New(agent.Options{Model: script, Middleware: []agent.Middleware{nemotronEntityResolutionGuard()}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "The current incident affects checkout-web." {
		t.Fatalf("messages = %#v", result.Messages)
	}
	if _, leaked := result.State["nemotron_entity_guard_fired"]; leaked {
		t.Fatalf("private guard state leaked: %#v", result.State)
	}
}

func TestNemotronEntityGuardPreNudgesAfterToolResult(t *testing.T) {
	current := message.ToolCall{ID: "current", Name: "get_current_incident_id", Arguments: json.RawMessage(`{}`)}
	relation := message.ToolCall{ID: "relation", Name: "get_incident_service", Arguments: json.RawMessage(`{"incident_id":41017}`)}
	first := message.Assistant("")
	first.ToolCalls = []message.ToolCall{current}
	second := message.Assistant("")
	second.ToolCalls = []message.ToolCall{relation}
	messages := []message.Message{
		message.Human("What service is affected by the current incident?"),
		first, message.Tool(current.ID, "41017"), second, message.Tool(relation.ID, "8514"),
	}
	update, err := nemotronEntityResolutionGuard().BeforeModel(context.Background(), mapState(messages), agent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, err := policyMessages(update[agent.MessagesKey])
	if err != nil || len(nudges) != 1 || !strings.Contains(nudges[0].TextContent(), "service_id 8514") {
		t.Fatalf("update = %#v, error = %v", update, err)
	}
}

func TestNemotronEntityGuardAcceptsResolvedDisplay(t *testing.T) {
	lookup := message.ToolCall{ID: "lookup", Name: "get_service_name", Arguments: json.RawMessage(`{"service_id":8514}`)}
	assistant := message.Assistant("")
	assistant.ToolCalls = []message.ToolCall{lookup}
	messages := []message.Message{message.Human("What service is selected?"), assistant, message.Tool(lookup.ID, "checkout-web"), message.Assistant("checkout-web")}
	update, err := nemotronEntityResolutionGuard().AfterAgent(context.Background(), mapState(messages), agent.Runtime{})
	if err != nil || len(update) != 0 {
		t.Fatalf("update = %#v, error = %v", update, err)
	}
}

func mapState(messages []message.Message) state.Values {
	return state.Values{agent.MessagesKey: messages}
}
