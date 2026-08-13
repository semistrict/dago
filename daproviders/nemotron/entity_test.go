package nemotron

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
)

func TestNemotronEntityGuardKeepsCurrentBranchBound(t *testing.T) {
	history := []damessage.Message{damessage.Human("What service is affected by the current incident?")}
	current := damessage.ToolCall{ID: "current", Name: "get_current_incident_id", Arguments: json.RawMessage(`{}`)}
	currentRequest := damessage.Assistant("")
	currentRequest.ToolCalls = []damessage.ToolCall{current}
	history = append(history, currentRequest, damessage.Tool(current.ID, "41017"))
	relation := damessage.ToolCall{ID: "relation", Name: "get_incident_service", Arguments: json.RawMessage(`{"incident_id":41017}`)}
	relationRequest := damessage.Assistant("")
	relationRequest.ToolCalls = []damessage.ToolCall{relation}
	history = append(history, relationRequest, damessage.Tool(relation.ID, "8514"))

	script := modeltest.New(damodel.Profile{},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("The current incident affects service 8514.")}},
		modeltest.Step{Check: func(request damodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Name != nemotronEntitySource || !strings.Contains(last.TextContent(), "service_id 8514") || !strings.Contains(last.TextContent(), "get_service_title") {
				return errors.New("entity resolution nudge missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("The current incident affects checkout-web.")}},
	)
	compiled := dagent.New(script, dagent.Options{Middleware: []dagent.Middleware{nemotronEntityResolutionGuard()}})

	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: history})
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
	current := damessage.ToolCall{ID: "current", Name: "get_current_incident_id", Arguments: json.RawMessage(`{}`)}
	relation := damessage.ToolCall{ID: "relation", Name: "get_incident_service", Arguments: json.RawMessage(`{"incident_id":41017}`)}
	first := damessage.Assistant("")
	first.ToolCalls = []damessage.ToolCall{current}
	second := damessage.Assistant("")
	second.ToolCalls = []damessage.ToolCall{relation}
	messages := []damessage.Message{
		damessage.Human("What service is affected by the current incident?"),
		first, damessage.Tool(current.ID, "41017"), second, damessage.Tool(relation.ID, "8514"),
	}
	update, err := nemotronEntityResolutionGuard().BeforeModel(context.Background(), mapState(messages), dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, err := policyMessages(update[dagent.MessagesKey])
	if err != nil || len(nudges) != 1 || !strings.Contains(nudges[0].TextContent(), "service_id 8514") {
		t.Fatalf("update = %#v, error = %v", update, err)
	}
}

func TestNemotronEntityGuardAcceptsResolvedDisplay(t *testing.T) {
	lookup := damessage.ToolCall{ID: "lookup", Name: "get_service_name", Arguments: json.RawMessage(`{"service_id":8514}`)}
	assistant := damessage.Assistant("")
	assistant.ToolCalls = []damessage.ToolCall{lookup}
	messages := []damessage.Message{damessage.Human("What service is selected?"), assistant, damessage.Tool(lookup.ID, "checkout-web"), damessage.Assistant("checkout-web")}
	update, err := nemotronEntityResolutionGuard().AfterAgent(context.Background(), mapState(messages), dagent.Runtime{})
	if err != nil || len(update) != 0 {
		t.Fatalf("update = %#v, error = %v", update, err)
	}
}

func mapState(messages []damessage.Message) dastate.Values {
	return dastate.Values{dagent.MessagesKey: messages}
}
