package daserver

import "encoding/json"

func defaultMessagesSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"messages":{"type":"array","items":{"description":"Base class for all types of messages in a conversation.","type":"array","items":{}},"langgraph_type":"messages"}},"$schema":"http://json-schema.org/draft-07/schema#"}`)
}

func defaultConfigSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","$schema":"http://json-schema.org/draft-07/schema#"}`)
}

func defaultDrawableGraph() DrawableGraph {
	return DrawableGraph{
		Nodes: []GraphNode{
			{ID: "__start__", Type: "schema"},
			{ID: "agent", Type: "runnable", Data: map[string]any{"name": "agent"}},
			{ID: "tools", Type: "runnable", Data: map[string]any{"name": "tools"}},
			{ID: "__end__", Type: "schema"},
		},
		Edges: []GraphEdge{
			{Source: "__start__", Target: "agent"},
			{Source: "agent", Target: "tools", Conditional: true},
			{Source: "tools", Target: "agent"},
			{Source: "agent", Target: "__end__", Conditional: true},
		},
	}
}
