// Package lifecycle applies bounded retention policy to one datalon assistant's
// sensitive local state.
package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/semistrict/dago/datalon/cron"
)

const (
	// SessionDeletionAcknowledgement is required for policies that can remove
	// provider authentication/session state.
	SessionDeletionAcknowledgement = "delete-channel-session-state"

	defaultCronRetention    = 30 * 24 * time.Hour
	defaultMediaRetention   = 24 * time.Hour
	defaultMaxCronJobs      = 1_000
	defaultMaxWalkEntries   = 10_000
	defaultMaxDepth         = 32
	defaultMaxReportEntries = 1_000
	defaultMaxArtifactBytes = int64(1 << 30)
	defaultMaxSelectedBytes = int64(8 << 30)
)

var (
	// ErrUnsafeState rejects linked, special, or escaping state paths.
	ErrUnsafeState = errors.New("datalon lifecycle encountered unsafe assistant state")
	// ErrLifecycleLimit rejects state walks that exceed configured finite bounds.
	ErrLifecycleLimit = errors.New("datalon lifecycle limit exceeded")
)

// CronStore is the narrow cron retention contract required by Manager.
type CronStore interface {
	List(context.Context, *cron.Origin) ([]cron.Job, error)
	PruneCompleted(context.Context, time.Duration, time.Time) ([]cron.Job, error)
}

// ArtifactKind identifies a caller-opted retention class.
type ArtifactKind string

const (
	ArtifactChannel ArtifactKind = "channel_artifact"
	ArtifactSession ArtifactKind = "channel_session"
	ArtifactTracing ArtifactKind = "tracing_artifact"
	// ArtifactInboundMedia identifies the pinned downloaded-media policy.
	ArtifactInboundMedia ArtifactKind = "inbound_media"
	// ArtifactCompletedCron identifies completed cron record audit entries.
	ArtifactCompletedCron ArtifactKind = "completed_cron"
)

// FilePolicy opts a conventional state subdirectory into age-based cleanup.
// Channel artifact roots must include channels/<provider>/artifacts; session
// roots stay beneath channels and tracing roots beneath traces/tracing. Session
// deletion requires the acknowledgement.
type FilePolicy struct {
	Kind            ArtifactKind
	RelativeRoot    string
	RetainFor       time.Duration
	Acknowledgement string
}

// Options controls retention and finite resource bounds. Zero values select
// the pinned 30-day cron, 24-hour inbound-media, 1 GiB artifact, and bounded
// walk/report defaults. Durable channel sessions and tracing are preserved
// unless explicitly added through FilePolicies.
type Options struct {
	CronRetention         time.Duration
	MediaRetention        time.Duration
	ImmediateCronCleanup  bool
	ImmediateMediaCleanup bool
	MaxCronJobs           int
	MaxWalkEntries        int
	MaxDepth              int
	MaxReportEntries      int
	MaxArtifactBytes      int64
	MaxSelectedBytes      int64
	FilePolicies          []FilePolicy
}

// Entry is a non-secret audit record. Reference is a stable digest of the
// relative path or cron job ID; raw names, prompts, and contents are omitted.
type Entry struct {
	Kind       ArtifactKind `json:"kind"`
	Reference  string       `json:"reference"`
	Bytes      int64        `json:"bytes,omitempty"`
	ModifiedAt time.Time    `json:"modified_at,omitempty"`
	Deleted    bool         `json:"deleted"`
}

// Report summarizes a dry run or cleanup without exposing paths or cron data.
type Report struct {
	DryRun             bool      `json:"dry_run"`
	At                 time.Time `json:"at"`
	CompletedCronJobs  int       `json:"completed_cron_jobs"`
	Files              int       `json:"files"`
	Bytes              int64     `json:"bytes"`
	EmptyDirectories   int       `json:"empty_directories"`
	SecuredFiles       int       `json:"secured_files"`
	SecuredDirectories int       `json:"secured_directories"`
	Entries            []Entry   `json:"entries,omitempty"`
	TruncatedEntries   int       `json:"truncated_entries,omitempty"`
}

// Manager owns retention policy for one explicit per-assistant state root.
type Manager struct {
	root     string
	cron     CronStore
	options  Options
	policies []preparedPolicy
	lock     chan struct{}
}

// New constructs a lifecycle manager without filesystem or cron I/O. stateRoot
// and cronStore are required positional dependencies; invalid static inputs panic.
func New(stateRoot string, cronStore CronStore, options Options) *Manager {
	stateRoot = filepath.Clean(strings.TrimSpace(stateRoot))
	if stateRoot == "." || !filepath.IsAbs(stateRoot) || filepath.Dir(stateRoot) == stateRoot {
		panic("datalon lifecycle: state root must be an absolute non-root path")
	}
	if isNil(cronStore) {
		panic("datalon lifecycle: cron store is required")
	}
	prepared, policies := prepareOptions(options)
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	return &Manager{root: stateRoot, cron: cronStore, options: prepared, policies: policies, lock: lock}
}

// DryRun returns the records that would be removed at the current UTC time.
func (manager *Manager) DryRun(ctx context.Context) (Report, error) {
	return manager.DryRunAt(ctx, time.Now())
}

// DryRunAt is DryRun with a deterministic clock override.
func (manager *Manager) DryRunAt(ctx context.Context, now time.Time) (Report, error) {
	return manager.run(ctx, now, true)
}

// Clean applies retention at the current UTC time.
func (manager *Manager) Clean(ctx context.Context) (Report, error) {
	return manager.CleanAt(ctx, time.Now())
}

// CleanAt is Clean with a deterministic clock override.
func (manager *Manager) CleanAt(ctx context.Context, now time.Time) (Report, error) {
	return manager.run(ctx, now, false)
}

func (manager *Manager) run(ctx context.Context, now time.Time, dryRun bool) (Report, error) {
	now = now.UTC()
	report := Report{DryRun: dryRun, At: now, Entries: []Entry{}}
	select {
	case <-ctx.Done():
		return report, ctx.Err()
	case <-manager.lock:
	}
	defer func() { manager.lock <- struct{}{} }()

	root, exists, err := manager.openStateRoot(dryRun)
	if err != nil || !exists {
		return report, err
	}
	defer root.Close()
	plans, observed, directories, err := manager.planFiles(ctx, root, now)
	if err != nil {
		return report, err
	}
	jobs, err := manager.cron.List(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("inspect completed cron jobs: %w", err)
	}
	if len(jobs) > manager.options.MaxCronJobs {
		return report, ErrLifecycleLimit
	}
	cronCandidates := completedJobs(jobs, now.Add(-manager.options.CronRetention))
	report.CompletedCronJobs = len(cronCandidates)
	for _, job := range cronCandidates {
		manager.addEntry(&report, Entry{Kind: ArtifactCompletedCron, Reference: auditReference(ArtifactCompletedCron, job.ID)})
	}
	for _, plan := range plans {
		report.Files++
		if plan.size > 0 && report.Bytes > manager.options.MaxSelectedBytes-plan.size {
			return report, ErrLifecycleLimit
		}
		report.Bytes += plan.size
		manager.addEntry(&report, Entry{
			Kind: plan.kind, Reference: auditReference(plan.kind, plan.path),
			Bytes: plan.size, ModifiedAt: plan.modified,
		})
	}
	if dryRun {
		return report, nil
	}
	if err := manager.secureState(ctx, root, observed, directories, &report); err != nil {
		return report, err
	}
	removedJobs, err := manager.cron.PruneCompleted(ctx, manager.options.CronRetention, now)
	if err != nil {
		return report, fmt.Errorf("prune completed cron jobs: %w", err)
	}
	report.CompletedCronJobs = len(removedJobs)
	report.Entries = report.Entries[:0]
	report.TruncatedEntries = 0
	for _, job := range removedJobs {
		manager.addEntry(&report, Entry{Kind: ArtifactCompletedCron, Reference: auditReference(ArtifactCompletedCron, job.ID), Deleted: true})
	}
	for _, plan := range plans {
		manager.addEntry(&report, Entry{
			Kind: plan.kind, Reference: auditReference(plan.kind, plan.path),
			Bytes: plan.size, ModifiedAt: plan.modified,
		})
	}
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		current, err := root.Lstat(plan.path)
		if err != nil || !current.Mode().IsRegular() || !os.SameFile(plan.identity, current) {
			return report, ErrUnsafeState
		}
		if err := root.Remove(plan.path); err != nil {
			return report, fmt.Errorf("remove retained artifact %s: %w", auditReference(plan.kind, plan.path), err)
		}
		manager.markReferenceDeleted(&report, auditReference(plan.kind, plan.path))
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := root.Remove(directories[index]); err == nil {
			report.EmptyDirectories++
		} else if !errors.Is(err, os.ErrNotExist) && !directoryNotEmpty(err) {
			return report, fmt.Errorf("remove empty artifact directory: %w", err)
		}
	}
	return report, nil
}

func (manager *Manager) openStateRoot(dryRun bool) (*os.Root, bool, error) {
	info, err := os.Lstat(manager.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, ErrUnsafeState
	}
	root, err := os.OpenRoot(manager.root)
	if err != nil {
		return nil, false, fmt.Errorf("open assistant state: %w", err)
	}
	if !dryRun {
		if err := root.Chmod(".", 0o700); err != nil {
			root.Close()
			return nil, false, fmt.Errorf("secure assistant state: %w", err)
		}
	}
	return root, true, nil
}

func completedJobs(jobs []cron.Job, cutoff time.Time) []cron.Job {
	result := make([]cron.Job, 0)
	for _, job := range jobs {
		reference := job.LastRunAt
		if reference.IsZero() {
			reference = job.CreatedAt
		}
		if !job.Enabled && job.NextRunAt.IsZero() && !reference.After(cutoff) {
			result = append(result, job)
		}
	}
	return result
}

func (manager *Manager) addEntry(report *Report, entry Entry) {
	if len(report.Entries) < manager.options.MaxReportEntries {
		report.Entries = append(report.Entries, entry)
	} else {
		report.TruncatedEntries++
	}
}

func (manager *Manager) markReferenceDeleted(report *Report, reference string) {
	for index := range report.Entries {
		if report.Entries[index].Reference == reference {
			report.Entries[index].Deleted = true
			return
		}
	}
}

func auditReference(kind ArtifactKind, value string) string {
	digest := sha256.Sum256([]byte(string(kind) + "\x00" + value))
	return hex.EncodeToString(digest[:16])
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var _ CronStore = (*cron.Store)(nil)
