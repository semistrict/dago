package dacode

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/modelconfig"
)

type recordingModelPreferences struct {
	mu          sync.Mutex
	defaultSpec string
	recentSpec  string
	err         error
}

func TestModelSelectorAvailabilityUsesLocalCredentialAndFactoryStatus(t *testing.T) {
	store := dacredential.NewStore(t.TempDir()+"/auth.json", time.Now, dacredential.Options{})
	if err := store.SetAPIKey(t.Context(), "anthropic", "fixture-"+"key", "", ""); err != nil {
		t.Fatal(err)
	}
	factory := func(context.Context, modelconfig.Spec, dacredential.Resolution, modelconfig.Construction) (damodel.Chat, error) {
		return nil, nil
	}
	resolver := modelconfig.NewResolver(store, func(string) (string, bool) { return "", false }, map[string]modelconfig.Factory{"anthropic": factory}, modelconfig.Options{})
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "anthropic:model", "thread", false, false, "")
	if err := model.configureModelProviderAvailability(t.Context(), resolver, true); err != nil {
		t.Fatal(err)
	}
	anthropic := model.modelProviderAvailability["anthropic"]
	if anthropic.Install != modelRequirementReady || anthropic.Credentials != modelRequirementReady {
		t.Fatalf("anthropic availability = %#v", anthropic)
	}
	oauth := model.modelProviderAvailability["openai_oauth"]
	if oauth.Install != modelRequirementReady || oauth.Credentials != modelRequirementReady {
		t.Fatalf("subscription availability = %#v", oauth)
	}
}

func (preferences *recordingModelPreferences) Default(context.Context) (string, error) {
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	return preferences.defaultSpec, preferences.err
}

func (preferences *recordingModelPreferences) Recent(context.Context) (string, error) {
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	return preferences.recentSpec, preferences.err
}

func (preferences *recordingModelPreferences) SetDefault(_ context.Context, spec string) error {
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	if preferences.err == nil {
		preferences.defaultSpec = spec
	}
	return preferences.err
}

func (preferences *recordingModelPreferences) ClearDefault(context.Context) (bool, error) {
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	if preferences.err != nil {
		return false, preferences.err
	}
	changed := preferences.defaultSpec != ""
	preferences.defaultSpec = ""
	return changed, nil
}

func (preferences *recordingModelPreferences) SetRecent(_ context.Context, spec string) error {
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	if preferences.err == nil {
		preferences.recentSpec = spec
	}
	return preferences.err
}

func TestModelSelectorIntegrationRollsBackFailedDefaultWrite(t *testing.T) {
	preferences := &recordingModelPreferences{defaultSpec: "provider:old", err: errors.New("write failed")}
	selector := newModelSelector([]modelSelectorEntry{
		{Spec: "provider:old", Recommended: true}, {Spec: "provider:new", Recommended: true},
	}, "provider:new", "provider:old")
	selector.selectSpec("provider:new")
	model := &tuiModel{
		ctx: context.Background(), modelSelector: selector, modelPreferences: preferences,
		modelDefaultSpec: "provider:old",
	}
	result := selector.handleKey("ctrl+s", 5)
	command := model.applyModelSelectorResult(result)
	if command == nil || model.modelDefaultSpec != "provider:new" || selector.defaultSpec != "provider:new" {
		t.Fatalf("optimistic write command=%v model=%q selector=%q", command != nil, model.modelDefaultSpec, selector.defaultSpec)
	}
	message := command().(modelPreferenceMsg)
	model.finishModelPreference(message)
	if model.modelDefaultSpec != "provider:old" || selector.defaultSpec != "provider:old" || selector.pendingWrite != nil {
		t.Fatalf("rollback model=%q selector=%q pending=%#v", model.modelDefaultSpec, selector.defaultSpec, selector.pendingWrite)
	}
}

func TestModelSelectorIntegrationRejectsStalePreferenceCompletion(t *testing.T) {
	preferences := &recordingModelPreferences{defaultSpec: "provider:old"}
	selector := newModelSelector([]modelSelectorEntry{
		{Spec: "provider:old", Recommended: true}, {Spec: "provider:new", Recommended: true},
	}, "provider:new", "provider:old")
	selector.selectSpec("provider:new")
	model := &tuiModel{
		ctx: context.Background(), modelSelector: selector, modelPreferences: preferences,
		modelDefaultSpec: "provider:old",
	}
	command := model.applyModelSelectorResult(selector.handleKey("ctrl+s", 5))
	message := command().(modelPreferenceMsg)
	selector.replaceDefault("provider:old")
	model.modelDefaultSpec = "provider:old"
	if notification := model.finishModelPreference(message); notification != nil {
		t.Fatal("stale completion produced a notification")
	}
	if model.modelDefaultSpec != "provider:old" || selector.defaultSpec != "provider:old" {
		t.Fatalf("stale completion changed defaults: model=%q selector=%q", model.modelDefaultSpec, selector.defaultSpec)
	}
}

func TestModelPreferenceControllerIsSafeForConcurrentAsyncWrites(t *testing.T) {
	preferences := &recordingModelPreferences{}
	const count = 32
	var wait sync.WaitGroup
	wait.Add(count)
	for index := range count {
		go func() {
			defer wait.Done()
			if err := preferences.SetRecent(context.Background(), fmt.Sprintf("provider:model-%02d", index)); err != nil {
				t.Errorf("set recent: %v", err)
			}
		}()
	}
	wait.Wait()
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	if validModelSelectorSpec(preferences.recentSpec) == "" {
		t.Fatalf("recent spec = %q", preferences.recentSpec)
	}
}

func TestModelPreferenceSequencerPreservesNewestIntent(t *testing.T) {
	sequence := newModelPreferenceSequencer()
	oldGeneration := sequence.begin("default")
	newGeneration := sequence.begin("default")
	stored := ""
	applied, err := sequence.apply(t.Context(), "default", newGeneration, func() error {
		stored = "provider:new"
		return nil
	})
	if err != nil || !applied {
		t.Fatalf("new write applied=%v error=%v", applied, err)
	}
	applied, err = sequence.apply(t.Context(), "default", oldGeneration, func() error {
		stored = "provider:old"
		return nil
	})
	if err != nil || applied || stored != "provider:new" {
		t.Fatalf("late old write applied=%v error=%v stored=%q", applied, err, stored)
	}

	sequence = newModelPreferenceSequencer()
	oldGeneration = sequence.begin("default")
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_, _ = sequence.apply(t.Context(), "default", oldGeneration, func() error {
			close(started)
			<-release
			stored = "provider:old"
			return nil
		})
		close(done)
	}()
	<-started
	newGeneration = sequence.begin("default")
	newDone := make(chan struct{})
	go func() {
		_, _ = sequence.apply(t.Context(), "default", newGeneration, func() error {
			stored = "provider:new"
			return nil
		})
		close(newDone)
	}()
	close(release)
	<-done
	<-newDone
	if stored != "provider:new" {
		t.Fatalf("new write did not follow in-progress old write: %q", stored)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	applied, err = sequence.apply(cancelled, "default", sequence.begin("default"), func() error {
		called = true
		return nil
	})
	if applied || called || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write applied=%v called=%v error=%v", applied, called, err)
	}
}

func TestSupersededPreferenceCompletionReleasesOnlyItsPendingSelector(t *testing.T) {
	selector := newModelSelector([]modelSelectorEntry{
		{Spec: "provider:old", Recommended: true}, {Spec: "provider:stale", Recommended: true}, {Spec: "provider:new", Recommended: true},
	}, "provider:stale", "provider:old")
	selector.selectSpec("provider:stale")
	write, accepted := selector.beginPreferenceWrite(selector.request(modelSelectorSetDefault, "provider:stale"))
	if !accepted {
		t.Fatal("stale fixture write was not accepted")
	}
	model := &tuiModel{deferredModelSelector: selector, modelDefaultSpec: "provider:new"}
	if command := model.finishModelPreference(modelPreferenceMsg{action: "default", write: write, superseded: true}); command != nil {
		t.Fatal("superseded completion produced a notification")
	}
	if model.deferredModelSelector != nil || selector.pendingWrite != nil || selector.defaultSpec != "provider:new" {
		t.Fatalf("superseded completion state selector=%#v pending=%#v default=%q", model.deferredModelSelector, selector.pendingWrite, selector.defaultSpec)
	}
}
