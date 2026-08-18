package dacode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/semistrict/dago/daconfig"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/daproviders/modelconfig"
)

const autoClassifierConfigKey = "models.approval"

const maxAutoClassifierCounterBytes = 4096

type autoClassifierCounters struct {
	ConsecutiveDenials         int    `json:"consecutive_denials"`
	TotalDenials               int    `json:"total_denials"`
	ConsecutiveUnavailable     int    `json:"consecutive_unavailable"`
	ClassifierIdentity         string `json:"classifier_identity,omitempty"`
	LastTurnID                 string `json:"last_turn_id,omitempty"`
	LastMode                   string `json:"last_mode,omitempty"`
	LastBatchID                string `json:"last_batch_id,omitempty"`
	ClassifierConfigFailedSpec string `json:"classifier_config_failed_spec,omitempty"`
}

const inheritedAutoClassifierIdentity = "main"

func (runner *dagoRunner) approvalClassifierIdentity(selection autoClassifierContext) string {
	if selection.Set && selection.Inherit {
		return inheritedAutoClassifierIdentity
	}
	spec := runner.reviewerSpec
	if selection.Set {
		spec = selection.Spec
	}
	if spec == "" {
		return inheritedAutoClassifierIdentity
	}
	return spec
}

type autoClassifierRunner interface {
	ValidateAutoClassifier(context.Context, string) (autoClassifierValidation, error)
}

type autoClassifierPreferenceController interface {
	Set(context.Context, string) error
	Clear(context.Context) error
}

type configAutoClassifierPreferences struct{ store *daconfig.Store }

func newAutoClassifierPreferences(store *daconfig.Store) autoClassifierPreferenceController {
	if store == nil {
		panic("dacode: classifier preference store is required")
	}
	return &configAutoClassifierPreferences{store: store}
}

func (preferences *configAutoClassifierPreferences) Set(ctx context.Context, spec string) error {
	return preferences.store.Set(ctx, autoClassifierConfigKey, spec)
}

func (preferences *configAutoClassifierPreferences) Clear(ctx context.Context) error {
	_, err := preferences.store.Unset(ctx, autoClassifierConfigKey)
	return err
}

func (runner *dagoRunner) ValidateAutoClassifier(ctx context.Context, spec string) (autoClassifierValidation, error) {
	if ctx == nil {
		panic("dacode: classifier validation context is required")
	}
	if runner == nil || runner.reviewerModel == nil {
		return autoClassifierValidation{}, errors.New("classifier runtime is unavailable")
	}
	model, err := runner.reviewerModel(ctx, spec)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return autoClassifierValidation{}, err
		}
		if errors.Is(err, modelconfig.ErrMissingCredential) {
			return autoClassifierValidation{ModelAvailable: true}, nil
		}
		return autoClassifierValidation{}, nil
	}
	support := autoClassifierStructuredUnsupported
	if model.Profile().ToolCalling {
		support = autoClassifierStructuredSupported
	}
	return autoClassifierValidation{
		ModelAvailable: true, CredentialsAvailable: true, StructuredOutput: support,
	}, nil
}

func (runner *dagoRunner) approvalReviewerWithSpec(ctx context.Context, selection autoClassifierContext) (*dagent.Agent, string, error) {
	if runner == nil {
		return nil, "", errors.New("automatic approval reviewer is not configured")
	}
	if selection.Set && selection.Inherit {
		if runner.mainReviewer == nil {
			return nil, "", errors.New("main-model approval reviewer is unavailable")
		}
		return runner.mainReviewer, "", nil
	}
	spec := runner.reviewerSpec
	if selection.Set {
		spec = selection.Spec
	}
	if spec == "" {
		if runner.mainReviewer == nil {
			return nil, "", errors.New("automatic approval reviewer is not configured")
		}
		return runner.mainReviewer, "", nil
	}
	if !selection.Set && runner.reviewer != nil {
		return runner.reviewer, spec, nil
	}
	if runner.reviewerModel == nil || runner.reviewBackend == nil {
		return nil, spec, errors.New("automatic approval reviewer is not configured")
	}
	model, err := runner.reviewerModel(ctx, spec)
	if err != nil {
		return nil, spec, fmt.Errorf("resolve automatic approval classifier: %w", err)
	}
	return newApprovalReviewer(model, runner.reviewBackend), spec, nil
}

func setupAutoClassifierCounters(ctx context.Context, database *sql.DB) error {
	if ctx == nil || database == nil {
		panic("dacode: classifier counter database dependencies are required")
	}
	_, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS auto_classifier_counters (
		thread_id TEXT PRIMARY KEY NOT NULL,
		value BLOB NOT NULL CHECK(length(value) <= 4096)
	)`)
	if err != nil {
		return fmt.Errorf("create classifier counter store: %w", err)
	}
	return nil
}

func (runner *dagoRunner) loadAutoClassifierCounters(ctx context.Context, threadID string) (autoClassifierCounters, error) {
	if runner == nil || runner.database == nil || ctx == nil || threadID == "" || len(threadID) > 512 {
		return autoClassifierCounters{}, errors.New("classifier counter key is invalid")
	}
	var payload []byte
	err := runner.database.QueryRowContext(ctx, `SELECT value FROM auto_classifier_counters WHERE thread_id = ?`, threadID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return autoClassifierCounters{}, nil
	}
	if err != nil {
		return autoClassifierCounters{}, err
	}
	if len(payload) == 0 || len(payload) > maxAutoClassifierCounterBytes {
		return autoClassifierCounters{}, errors.New("classifier counters are malformed")
	}
	var counters autoClassifierCounters
	if json.Unmarshal(payload, &counters) != nil || !validAutoClassifierCounters(counters) {
		return autoClassifierCounters{}, errors.New("classifier counters are malformed")
	}
	return counters, nil
}

func (runner *dagoRunner) saveAutoClassifierCounters(ctx context.Context, threadID string, counters autoClassifierCounters) error {
	if runner == nil || runner.database == nil || ctx == nil || threadID == "" || len(threadID) > 512 || !validAutoClassifierCounters(counters) {
		return errors.New("classifier counters are invalid")
	}
	payload, err := json.Marshal(counters)
	if err != nil || len(payload) > maxAutoClassifierCounterBytes {
		return errors.New("classifier counters exceed their bound")
	}
	_, err = runner.database.ExecContext(ctx, `INSERT INTO auto_classifier_counters(thread_id, value) VALUES(?, ?)
		ON CONFLICT(thread_id) DO UPDATE SET value = excluded.value`, threadID, payload)
	return err
}

func validAutoClassifierCounters(counters autoClassifierCounters) bool {
	return counters.ConsecutiveDenials >= 0 && counters.ConsecutiveDenials <= autoTotalDenialFallback &&
		counters.TotalDenials >= 0 && counters.TotalDenials <= autoTotalDenialFallback &&
		counters.ConsecutiveUnavailable >= 0 && counters.ConsecutiveUnavailable <= autoUnavailableFallback &&
		len(counters.ClassifierIdentity) <= maxAutoClassifierSpecBytes &&
		len(counters.LastTurnID) <= 64 && len(counters.LastMode) <= 16 &&
		len(counters.LastBatchID) <= 64 && len(counters.ClassifierConfigFailedSpec) <= maxAutoClassifierSpecBytes
}

func (runner *reloadableRunner) ValidateAutoClassifier(ctx context.Context, spec string) (autoClassifierValidation, error) {
	current := runner.current()
	capability, ok := current.(autoClassifierRunner)
	if !ok {
		return autoClassifierValidation{}, errors.New("classifier runtime is unavailable")
	}
	return capability.ValidateAutoClassifier(ctx, spec)
}
