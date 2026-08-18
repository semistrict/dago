package dacode

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/semistrict/dago/damodel"
)

const defaultRubricMaxIterations = 3

type rubricModelResolver func(context.Context, string) (damodel.Chat, error)

type rubricSettings struct {
	sync.RWMutex
	defaultModel     damodel.Chat
	defaultModelSpec string
	model            damodel.Chat
	modelSpec        string
	maxIterations    int
	resolve          rubricModelResolver
}

func newRubricSettings(defaultModel damodel.Chat, defaultModelSpec string, resolve rubricModelResolver) *rubricSettings {
	if defaultModel == nil || resolve == nil {
		panic("rubric settings require a default model and resolver")
	}
	return &rubricSettings{
		defaultModel: defaultModel, defaultModelSpec: strings.TrimSpace(defaultModelSpec),
		maxIterations: defaultRubricMaxIterations, resolve: resolve,
	}
}

func (settings *rubricSettings) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	return settings.currentModel().Invoke(ctx, request)
}

func (settings *rubricSettings) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	return settings.currentModel().Stream(ctx, request)
}

func (settings *rubricSettings) Profile() damodel.Profile { return settings.currentModel().Profile() }

func (settings *rubricSettings) currentModel() damodel.Chat {
	settings.RLock()
	defer settings.RUnlock()
	if settings.model != nil {
		return settings.model
	}
	return settings.defaultModel
}

func (settings *rubricSettings) Values() (string, int) {
	settings.RLock()
	defer settings.RUnlock()
	model := settings.modelSpec
	if model == "" {
		model = settings.defaultModelSpec
	}
	return model, settings.maxIterations
}

func (settings *rubricSettings) SetModel(ctx context.Context, modelSpec string) error {
	if settings == nil {
		return errors.New("rubric settings are unavailable")
	}
	modelSpec = strings.TrimSpace(modelSpec)
	if modelSpec == "" || strings.EqualFold(modelSpec, "clear") {
		settings.Lock()
		settings.model = nil
		settings.modelSpec = ""
		settings.Unlock()
		return nil
	}
	model, err := settings.resolve(ctx, modelSpec)
	if err != nil {
		return err
	}
	if model == nil {
		return errors.New("rubric model resolver returned no model")
	}
	settings.Lock()
	settings.model = model
	settings.modelSpec = modelSpec
	settings.Unlock()
	return nil
}

func (settings *rubricSettings) SetMaxIterations(value int) error {
	if settings == nil {
		return errors.New("rubric settings are unavailable")
	}
	if value < 0 {
		return errors.New("rubric max iterations cannot be negative")
	}
	if value == 0 {
		value = defaultRubricMaxIterations
	}
	settings.Lock()
	settings.maxIterations = value
	settings.Unlock()
	return nil
}

func (settings *rubricSettings) MaxIterations() int {
	if settings == nil {
		return defaultRubricMaxIterations
	}
	settings.RLock()
	defer settings.RUnlock()
	return settings.maxIterations
}
