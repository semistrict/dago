package dacode

import (
	"context"
	"errors"
	"testing"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func TestRubricSettingsSwitchAndClearModel(t *testing.T) {
	base := modeltest.New(damodel.Profile{Provider: "fixture", Model: "base"}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("base")}})
	alternate := modeltest.New(damodel.Profile{Provider: "fixture", Model: "grader"}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("grader")}})
	settings := newRubricSettings(base, "fixture:base", func(_ context.Context, spec string) (damodel.Chat, error) {
		if spec != "fixture:grader" {
			return nil, errors.New("unknown model")
		}
		return alternate, nil
	})
	if model, iterations := settings.Values(); model != "fixture:base" || iterations != defaultRubricMaxIterations {
		t.Fatalf("defaults = %q, %d", model, iterations)
	}
	if err := settings.SetModel(t.Context(), "fixture:grader"); err != nil {
		t.Fatal(err)
	}
	response, err := settings.Invoke(t.Context(), damodel.Request{})
	if err != nil || response.Message.TextContent() != "grader" {
		t.Fatalf("alternate response = %#v, %v", response, err)
	}
	if err := settings.SetMaxIterations(7); err != nil {
		t.Fatal(err)
	}
	if model, iterations := settings.Values(); model != "fixture:grader" || iterations != 7 {
		t.Fatalf("overrides = %q, %d", model, iterations)
	}
	if err := settings.SetModel(t.Context(), "clear"); err != nil {
		t.Fatal(err)
	}
	response, err = settings.Invoke(t.Context(), damodel.Request{})
	if err != nil || response.Message.TextContent() != "base" {
		t.Fatalf("cleared response = %#v, %v", response, err)
	}
}

func TestRubricSettingsFailClosedOnInvalidChanges(t *testing.T) {
	base := modeltest.New(damodel.Profile{Provider: "fixture", Model: "base"})
	settings := newRubricSettings(base, "fixture:base", func(context.Context, string) (damodel.Chat, error) {
		return nil, errors.New("unavailable")
	})
	if err := settings.SetModel(t.Context(), "missing"); err == nil {
		t.Fatal("missing model switch succeeded")
	}
	if err := settings.SetMaxIterations(-1); err == nil {
		t.Fatal("negative max iterations succeeded")
	}
	if model, iterations := settings.Values(); model != "fixture:base" || iterations != defaultRubricMaxIterations {
		t.Fatalf("invalid changes mutated settings: %q, %d", model, iterations)
	}
}
