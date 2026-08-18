package datool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func panicMessage(t *testing.T, function func()) string {
	t.Helper()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		function()
	}()
	if recovered == nil {
		t.Fatal("function did not panic")
	}
	return fmt.Sprint(recovered)
}

type generatedToolFilter struct {
	Field string `json:"field" description:"Field to compare"`
	Value string `json:"value"`
}

type generatedToolInput struct {
	Query   string                `json:"query" description:"Text to search for, including punctuation" jsonschema:"minLength=1"`
	Limit   int                   `json:"limit,omitempty" jsonschema:"minimum=1,maximum=100,default=10"`
	Mode    string                `json:"mode,omitempty" jsonschema:"enum=fast|thorough"`
	Filters []generatedToolFilter `json:"filters,omitempty" jsonschema:"minItems=1"`
	Exact   *bool                 `json:"exact,omitempty"`
	Ignored string                `json:"-"`
}

func TestSchemaDerivesObjectFromStructTags(t *testing.T) {
	raw, err := Schema[generatedToolInput]()
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Type                 string                     `json:"type"`
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" || schema.AdditionalProperties {
		t.Fatalf("schema = %s", raw)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "query" {
		t.Fatalf("required = %#v", schema.Required)
	}
	if _, exists := schema.Properties["Ignored"]; exists || len(schema.Properties) != 5 {
		t.Fatalf("properties = %#v", schema.Properties)
	}
	query := string(schema.Properties["query"])
	if !strings.Contains(query, `"minLength":1`) || !strings.Contains(query, "including punctuation") {
		t.Fatalf("query schema = %s", query)
	}
	mode := string(schema.Properties["mode"])
	if !strings.Contains(mode, `"enum":["fast","thorough"]`) {
		t.Fatalf("mode schema = %s", mode)
	}
	exact := string(schema.Properties["exact"])
	if !strings.Contains(exact, `"type":"null"`) || !strings.Contains(exact, `"type":"boolean"`) {
		t.Fatalf("exact schema = %s", exact)
	}
}

func TestNewGeneratesValidatesAndDecodesTypedInput(t *testing.T) {
	var received generatedToolInput
	created := New("search", "Search indexed records.", func(
		ctx context.Context,
		input generatedToolInput,
	) (Result, error) {
		received = input
		runtime, ok := RuntimeFromContext(ctx)
		if !ok {
			return Result{}, errors.New("runtime missing from context")
		}
		return TextResult(input.Query + ":" + runtime.CallID), nil
	})
	definition := created.Definition()
	if err := definition.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(definition.InputSchema), `"additionalProperties":false`) {
		t.Fatalf("input schema = %s", definition.InputSchema)
	}

	result, err := created.Execute(context.Background(), json.RawMessage(`{
		"query":"needle","limit":3,"mode":"fast","filters":[{"field":"name","value":"go"}]
	}`), Runtime{CallID: "call-7"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content[0].Text != "needle:call-7" || received.Limit != 3 || len(received.Filters) != 1 {
		t.Fatalf("result = %#v, input = %#v", result, received)
	}

	for name, arguments := range map[string]string{
		"missing required": `{}`,
		"constraint":       `{"query":""}`,
		"unknown field":    `{"query":"needle","extra":true}`,
		"wrong type":       `{"query":"needle","limit":"many"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := created.Execute(context.Background(), json.RawMessage(arguments), Runtime{}); !errors.Is(err, ErrInvalidArguments) {
				t.Fatalf("Execute() error = %v, want %v", err, ErrInvalidArguments)
			}
		})
	}

	legacy, err := created.Execute(context.Background(), json.RawMessage(`"{\"query\":\"legacy\"}"`), Runtime{})
	if err != nil || legacy.Content[0].Text != "legacy:" {
		t.Fatalf("legacy result = %#v, %v", legacy, err)
	}
}

func TestNewRejectsInvalidInputTypesAndTags(t *testing.T) {
	if _, err := Schema[int](); err == nil {
		t.Fatal("Schema[int]() succeeded")
	}
	type invalid struct {
		Value string `json:"value" jsonschema:"minLenght=1"`
	}
	if _, err := Schema[invalid](); err == nil || !strings.Contains(err.Error(), "minLenght") {
		t.Fatalf("Schema[invalid]() error = %v", err)
	}
	type invalidNullable struct {
		Value string `json:"value" jsonschema:"nullable"`
	}
	if _, err := Schema[invalidNullable](); err == nil || !strings.Contains(err.Error(), "nullable") {
		t.Fatalf("Schema[invalidNullable]() error = %v", err)
	}
	type quotedNumber struct {
		Value int `json:"value,string"`
	}
	if _, err := Schema[quotedNumber](); err == nil || !strings.Contains(err.Error(), "string option") {
		t.Fatalf("Schema[quotedNumber]() error = %v", err)
	}
	if message := panicMessage(t, func() { New("bad", "Bad.", Handler[generatedToolInput, Result](nil)) }); !strings.Contains(message, "handler is required") {
		t.Fatalf("New() panic = %q", message)
	}
}

func TestNewConvertsHandlerReturnValues(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		created := New("greet", "Return a greeting.", func(context.Context, struct{}) (string, error) {
			return "hello", nil
		})
		result, err := created.Execute(context.Background(), json.RawMessage(`{}`), Runtime{})
		if err != nil {
			t.Fatal(err)
		}
		if got := result.Content[0].Text; got != "hello" {
			t.Fatalf("text = %q", got)
		}
	})

	t.Run("JSON value", func(t *testing.T) {
		type output struct {
			Answer int `json:"answer"`
		}
		created := New("answer", "Return an answer.", func(context.Context, struct{}) (output, error) {
			return output{Answer: 42}, nil
		})
		result, err := created.Execute(context.Background(), json.RawMessage(`{}`), Runtime{})
		if err != nil {
			t.Fatal(err)
		}
		if got := result.Content[0].Text; got != `{"answer":42}` {
			t.Fatalf("text = %q", got)
		}
	})

	t.Run("Result", func(t *testing.T) {
		created := New("native", "Return a native result.", func(context.Context, struct{}) (Result, error) {
			return Result{Artifact: json.RawMessage(`{"native":true}`), Update: map[string]any{"done": true}}, nil
		})
		result, err := created.Execute(context.Background(), json.RawMessage(`{}`), Runtime{})
		if err != nil {
			t.Fatal(err)
		}
		if string(result.Artifact) != `{"native":true}` || result.Update["done"] != true {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		created := New("unsupported", "Return an unsupported value.", func(context.Context, struct{}) (chan int, error) {
			return make(chan int), nil
		})
		if _, err := created.Execute(context.Background(), json.RawMessage(`{}`), Runtime{}); err == nil || !strings.Contains(err.Error(), "convert tool result to JSON") {
			t.Fatalf("Execute() error = %v", err)
		}
	})
}

func TestNewTransformsGeneratedSchema(t *testing.T) {
	type input struct {
		Kind string `json:"kind"`
	}
	created := New("choose", "Choose a configured kind.", func(_ context.Context, input input) (string, error) {
		return input.Kind, nil
	}, WithTransformSchema(func(schema json.RawMessage) (json.RawMessage, error) {
		var document map[string]any
		if err := json.Unmarshal(schema, &document); err != nil {
			return nil, err
		}
		properties := document["properties"].(map[string]any)
		kind := properties["kind"].(map[string]any)
		kind["enum"] = []string{"worker", "reviewer"}
		return json.Marshal(document)
	}))
	if !strings.Contains(string(created.Definition().InputSchema), `"enum":["worker","reviewer"]`) {
		t.Fatalf("schema = %s", created.Definition().InputSchema)
	}
	if _, err := created.Execute(context.Background(), json.RawMessage(`{"kind":"worker"}`), Runtime{}); err != nil {
		t.Fatal(err)
	}
	result, err := created.Execute(context.Background(), json.RawMessage(`{"kind":"unknown"}`), Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content[0].Text != "unknown" {
		t.Fatalf("result = %#v", result)
	}
}

func TestNewRejectsInvalidSchemaTransforms(t *testing.T) {
	type input struct{}
	handler := func(context.Context, input) (string, error) { return "ok", nil }
	if message := panicMessage(t, func() { New("nil-option", "Reject a nil option.", handler, nil) }); !strings.Contains(message, "option 0 is nil") {
		t.Fatalf("New() nil option panic = %q", message)
	}
	if message := panicMessage(t, func() { New("nil-transform", "Reject a nil transform.", handler, WithTransformSchema(nil)) }); !strings.Contains(message, "schema transform is required") {
		t.Fatalf("New() nil transform panic = %q", message)
	}
	if message := panicMessage(t, func() {
		New("invalid-transform", "Reject invalid transformed JSON.", handler, WithTransformSchema(func(json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{`), nil
		}))
	}); !strings.Contains(message, "returned invalid JSON") {
		t.Fatalf("New() invalid transform panic = %q", message)
	}
}

func TestPropertyOptionsCustomizeGeneratedSchema(t *testing.T) {
	type child struct {
		Value *int `json:"value,omitempty"`
	}
	type input struct {
		Child   child  `json:"child"`
		Choice  string `json:"choice"`
		Removed string `json:"removed"`
	}
	created := New("properties", "Customize generated properties.", func(context.Context, input) (string, error) {
		return "ok", nil
	},
		WithPropertyType("child.value", "string"),
		WithPropertyEnum("choice", "one", "two"),
		WithPropertyValue("choice", "description", "Configured choice."),
		WithPropertySchema("child", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"value": map[string]any{"type": "string"},
			},
			"additionalProperties": false,
		}),
		WithoutProperty("removed"),
	)
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(created.Definition().InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, exists := schema.Properties["removed"]; exists {
		t.Fatalf("removed property remains in %s", created.Definition().InputSchema)
	}
	if strings.Contains(strings.Join(schema.Required, ","), "removed") {
		t.Fatalf("removed property remains required in %s", created.Definition().InputSchema)
	}
	choice := string(schema.Properties["choice"])
	if !strings.Contains(choice, `"enum":["one","two"]`) || !strings.Contains(choice, "Configured choice.") {
		t.Fatalf("choice schema = %s", choice)
	}
	childSchema := string(schema.Properties["child"])
	if !strings.Contains(childSchema, `"value":{"type":"string"}`) {
		t.Fatalf("child schema = %s", childSchema)
	}
}

func TestSchemaMatchesByteSliceJSONEncoding(t *testing.T) {
	type input struct {
		Payload []byte `json:"payload"`
	}
	raw, err := Schema[input]()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"contentEncoding":"base64"`) {
		t.Fatalf("schema = %s", raw)
	}
}
