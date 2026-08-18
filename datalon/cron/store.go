// Package cron provides persistent minute-granularity jobs for datalon hosts.
package cron

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/datalon"
)

const (
	defaultMaxJobs      = 1000
	defaultMaxFileBytes = 8 << 20
	defaultMaxPrompt    = 1 << 20
	defaultMaxName      = 256
	defaultMaxError     = 4096
)

var (
	ErrInvalidJob  = errors.New("invalid cron job")
	ErrJobNotFound = errors.New("cron job not found in the current conversation")
	ErrStoreBound  = errors.New("cron store limit exceeded")
)

// Kind identifies one-shot and recurring schedules.
type Kind string

const (
	OneShot   Kind = "one_shot"
	Recurring Kind = "recurring"
)

// Status is the persisted result of the last claimed run.
type Status string

const (
	StatusOK    Status = "ok"
	StatusError Status = "error"
)

// Schedule is a minute-granularity delay or interval.
type Schedule struct {
	Kind    Kind   `json:"kind"`
	Minutes int    `json:"minutes"`
	Display string `json:"display"`
}

// ParseSchedule parses forms such as "in 30m", "every 15m", and "every 2h".
func ParseSchedule(value string) (Schedule, error) {
	display := value
	parts := strings.Fields(strings.ToLower(strings.TrimSpace(value)))
	if len(parts) != 2 || (parts[0] != "in" && parts[0] != "every") {
		return Schedule{}, fmt.Errorf("%w: schedule must look like 'in 30m' or 'every 15m'", ErrInvalidJob)
	}
	unit := parts[1]
	multiplier := 1
	switch {
	case strings.HasSuffix(unit, "m"):
		unit = strings.TrimSuffix(unit, "m")
	case strings.HasSuffix(unit, "h"):
		unit = strings.TrimSuffix(unit, "h")
		multiplier = 60
	default:
		return Schedule{}, fmt.Errorf("%w: schedule duration must use m or h", ErrInvalidJob)
	}
	amount, err := strconv.Atoi(unit)
	if err != nil || amount <= 0 || amount > 525600 {
		return Schedule{}, fmt.Errorf("%w: schedule duration must be a bounded positive integer", ErrInvalidJob)
	}
	minutes := amount * multiplier
	if minutes > 525600 {
		return Schedule{}, fmt.Errorf("%w: schedule duration exceeds one year", ErrInvalidJob)
	}
	kind := OneShot
	if parts[0] == "every" {
		kind = Recurring
	}
	return Schedule{Kind: kind, Minutes: minutes, Display: display}, nil
}

// Origin scopes job management and delivery to one channel conversation.
type Origin struct {
	ConversationID string `json:"conversation_id"`
	ChannelID      string `json:"channel,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
}

// Repeat is an optional recurring-attempt cap. Times zero means unlimited.
type Repeat struct {
	Times     int `json:"times,omitempty"`
	Completed int `json:"completed"`
}

// Job is the stable jobs.json record.
type Job struct {
	ID          string    `json:"id"`
	AssistantID string    `json:"assistant_id"`
	Name        string    `json:"name"`
	Prompt      string    `json:"prompt"`
	Schedule    Schedule  `json:"schedule"`
	Repeat      Repeat    `json:"repeat"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	NextRunAt   time.Time `json:"next_run_at,omitzero"`
	LastRunAt   time.Time `json:"last_run_at,omitzero"`
	LastStatus  Status    `json:"last_status,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Origin      Origin    `json:"origin"`
}

type diskFile struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"`
}

// Options contains optional resource limits. Zero fields select finite defaults.
type Options struct {
	MaxJobs        int
	MaxFileBytes   int64
	MaxPromptBytes int
	MaxNameBytes   int
	MaxErrorBytes  int
}

// Store is an assistant-scoped, JSON-backed job store.
type Store struct {
	assistantID string
	dir         string
	path        string
	options     Options
	mu          sync.Mutex
}

// NewStore constructs a store for an explicit assistant and directory. Invalid
// static identifiers or an empty directory panic; file failures occur on methods.
func NewStore(assistantID, directory string, options Options) *Store {
	if !safeID(assistantID) {
		panic("datalon/cron: invalid assistant ID")
	}
	if strings.TrimSpace(directory) == "" {
		panic("datalon/cron: empty store directory")
	}
	if options.MaxJobs < 0 || options.MaxFileBytes < 0 || options.MaxPromptBytes < 0 || options.MaxNameBytes < 0 || options.MaxErrorBytes < 0 {
		panic("datalon/cron: store limits cannot be negative")
	}
	if options.MaxJobs == 0 {
		options.MaxJobs = defaultMaxJobs
	}
	if options.MaxFileBytes == 0 {
		options.MaxFileBytes = defaultMaxFileBytes
	}
	if options.MaxPromptBytes == 0 {
		options.MaxPromptBytes = defaultMaxPrompt
	}
	if options.MaxNameBytes == 0 {
		options.MaxNameBytes = defaultMaxName
	}
	if options.MaxErrorBytes == 0 {
		options.MaxErrorBytes = defaultMaxError
	}
	return &Store{assistantID: assistantID, dir: directory, path: filepath.Join(directory, "jobs.json"), options: options}
}

// NewStoreForConfig constructs the conventional cron/jobs.json store below one
// datalon assistant state directory. Config's zero value selects the default
// assistant and ~/.deepagents state root.
func NewStoreForConfig(config datalon.Config, options Options) *Store {
	assistantID := config.AssistantID
	if assistantID == "" {
		assistantID = "default"
	}
	return NewStore(assistantID, filepath.Join(config.StateDir(), "cron"), options)
}

// Path returns the jobs.json path without creating it.
func (store *Store) Path() string { return store.path }

// CreateOptions contains optional job fields.
type CreateOptions struct {
	Name        string
	RepeatTimes int
	Now         time.Time
}

// Create validates and persists a new job.
func (store *Store) Create(ctx context.Context, prompt string, schedule Schedule, origin Origin, options CreateOptions) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	if err := store.validateJobInput(prompt, options.Name, schedule, origin, options.RepeatTimes); err != nil {
		return Job{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.readLocked()
	if err != nil {
		return Job{}, err
	}
	if len(jobs) >= store.options.MaxJobs {
		return Job{}, ErrStoreBound
	}
	now := utcNow(options.Now)
	id, err := randomID()
	if err != nil {
		return Job{}, fmt.Errorf("create cron job ID: %w", err)
	}
	job := Job{
		ID: id, AssistantID: store.assistantID, Name: options.Name, Prompt: prompt,
		Schedule: schedule, Repeat: Repeat{Times: options.RepeatTimes}, Enabled: true,
		CreatedAt: now, NextRunAt: now.Add(time.Duration(schedule.Minutes) * time.Minute), Origin: origin,
	}
	jobs = append(jobs, job)
	if err := store.writeLocked(jobs); err != nil {
		return Job{}, err
	}
	return job, nil
}

// List returns creation-ordered jobs, optionally scoped to an origin.
func (store *Store) List(ctx context.Context, origin *Origin) ([]Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.readLocked()
	if err != nil {
		return nil, err
	}
	if origin == nil {
		return jobs, nil
	}
	result := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		if sameOrigin(job.Origin, *origin) {
			result = append(result, job)
		}
	}
	return result, nil
}

// PruneCompleted atomically removes disabled jobs with no next run whose last
// run (or creation time when never run) is at least retainFor old. A zero
// retention removes every completed job. The returned records are owned copies.
func (store *Store) PruneCompleted(ctx context.Context, retainFor time.Duration, now time.Time) ([]Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if retainFor < 0 {
		return nil, fmt.Errorf("%w: cron retention window cannot be negative", ErrInvalidJob)
	}
	cutoff := utcNow(now).Add(-retainFor)
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.readLocked()
	if err != nil {
		return nil, err
	}
	kept := make([]Job, 0, len(jobs))
	removed := make([]Job, 0)
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reference := job.LastRunAt
		if reference.IsZero() {
			reference = job.CreatedAt
		}
		if !job.Enabled && job.NextRunAt.IsZero() && !reference.After(cutoff) {
			removed = append(removed, job)
			continue
		}
		kept = append(kept, job)
	}
	if len(removed) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := store.writeLocked(kept); err != nil {
			return nil, err
		}
	}
	return removed, nil
}

// EditOptions contains optional replacement values. Nil fields are unchanged.
type EditOptions struct {
	Name        *string
	Prompt      *string
	Schedule    *Schedule
	Enabled     *bool
	RepeatTimes *int
	Now         time.Time
}

// Edit updates a job only within the supplied conversation scope.
func (store *Store) Edit(ctx context.Context, jobID string, origin Origin, options EditOptions) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.readLocked()
	if err != nil {
		return Job{}, err
	}
	for index, job := range jobs {
		if job.ID != jobID || !sameOrigin(job.Origin, origin) {
			continue
		}
		updated := job
		if options.Name != nil {
			updated.Name = *options.Name
		}
		if options.Prompt != nil {
			updated.Prompt = *options.Prompt
		}
		if options.Schedule != nil {
			updated.Schedule = *options.Schedule
			updated.NextRunAt = utcNow(options.Now).Add(time.Duration(updated.Schedule.Minutes) * time.Minute)
		}
		if options.Enabled != nil {
			updated.Enabled = *options.Enabled
			if *options.Enabled && updated.NextRunAt.IsZero() {
				updated.NextRunAt = utcNow(options.Now).Add(time.Duration(updated.Schedule.Minutes) * time.Minute)
			}
		}
		if options.RepeatTimes != nil {
			updated.Repeat = Repeat{Times: *options.RepeatTimes}
		}
		if err := store.validateJobInput(updated.Prompt, updated.Name, updated.Schedule, updated.Origin, updated.Repeat.Times); err != nil {
			return Job{}, err
		}
		jobs[index] = updated
		if err := store.writeLocked(jobs); err != nil {
			return Job{}, err
		}
		return updated, nil
	}
	return Job{}, ErrJobNotFound
}

// Remove deletes a job only within the supplied conversation scope.
func (store *Store) Remove(ctx context.Context, jobID string, origin Origin) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.readLocked()
	if err != nil {
		return Job{}, err
	}
	for index, job := range jobs {
		if job.ID == jobID && sameOrigin(job.Origin, origin) {
			jobs = append(jobs[:index], jobs[index+1:]...)
			if err := store.writeLocked(jobs); err != nil {
				return Job{}, err
			}
			return job, nil
		}
	}
	return Job{}, ErrJobNotFound
}

func (store *Store) due(ctx context.Context, now time.Time, limit int) ([]Job, error) {
	jobs, err := store.List(ctx, nil)
	if err != nil {
		return nil, err
	}
	due := make([]Job, 0)
	for _, job := range jobs {
		if job.Enabled && !job.NextRunAt.IsZero() && !job.NextRunAt.After(now) {
			due = append(due, job)
		}
	}
	sort.SliceStable(due, func(left, right int) bool { return due[left].NextRunAt.Before(due[right].NextRunAt) })
	if len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

func (store *Store) claim(ctx context.Context, jobID string, now time.Time) (Job, bool, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.readLocked()
	if err != nil {
		return Job{}, false, err
	}
	for index, job := range jobs {
		if job.ID != jobID || !job.Enabled || job.NextRunAt.IsZero() || job.NextRunAt.After(now) {
			continue
		}
		if job.Schedule.Kind == OneShot {
			job.Enabled, job.NextRunAt = false, time.Time{}
		} else {
			job.Repeat.Completed++
			if job.Repeat.Times > 0 && job.Repeat.Completed >= job.Repeat.Times {
				job.Enabled, job.NextRunAt = false, time.Time{}
			} else {
				interval := time.Duration(job.Schedule.Minutes) * time.Minute
				for !job.NextRunAt.After(now) {
					job.NextRunAt = job.NextRunAt.Add(interval)
				}
			}
		}
		jobs[index] = job
		if err := store.writeLocked(jobs); err != nil {
			return Job{}, false, err
		}
		return job, true, nil
	}
	return Job{}, false, nil
}

func (store *Store) mark(ctx context.Context, jobID string, status Status, errorText string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.readLocked()
	if err != nil {
		return err
	}
	for index := range jobs {
		if jobs[index].ID == jobID {
			jobs[index].LastRunAt = utcNow(now)
			jobs[index].LastStatus = status
			jobs[index].LastError = bounded(errorText, store.options.MaxErrorBytes)
			return store.writeLocked(jobs)
		}
	}
	return nil
}

func (store *Store) validateJobInput(prompt, name string, schedule Schedule, origin Origin, repeat int) error {
	if prompt == "" || len(prompt) > store.options.MaxPromptBytes || len(name) > store.options.MaxNameBytes {
		return ErrInvalidJob
	}
	if schedule.Minutes <= 0 || schedule.Minutes > 525600 || (schedule.Kind != OneShot && schedule.Kind != Recurring) {
		return ErrInvalidJob
	}
	if len(schedule.Display) > 256 {
		return ErrInvalidJob
	}
	if schedule.Kind == OneShot && repeat != 0 || repeat < 0 || repeat > 1_000_000 {
		return ErrInvalidJob
	}
	if origin.ConversationID == "" || len(origin.ConversationID) > 1024 || len(origin.ChannelID) > 128 || len(origin.MessageID) > 1024 {
		return ErrInvalidJob
	}
	return nil
}

func (store *Store) readLocked() ([]Job, error) {
	if err := store.ensureDir(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return []Job{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect cron jobs: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > store.options.MaxFileBytes {
		return nil, fmt.Errorf("%w: jobs file is not a bounded regular file", ErrStoreBound)
	}
	file, err := os.Open(store.path)
	if err != nil {
		return nil, fmt.Errorf("open cron jobs: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, store.options.MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read cron jobs: %w", err)
	}
	if int64(len(data)) > store.options.MaxFileBytes {
		return nil, ErrStoreBound
	}
	var persisted diskFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&persisted); err != nil {
		return nil, fmt.Errorf("decode cron jobs: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("decode cron jobs: trailing JSON data")
	}
	if persisted.Version != 1 {
		return nil, fmt.Errorf("decode cron jobs: unsupported version %d", persisted.Version)
	}
	jobs := persisted.Jobs
	if len(jobs) > store.options.MaxJobs {
		return nil, ErrStoreBound
	}
	for _, job := range jobs {
		if job.AssistantID != store.assistantID || !safeID(job.ID) || store.validateStoredJob(job) != nil {
			return nil, ErrInvalidJob
		}
	}
	return jobs, nil
}

func (store *Store) validateStoredJob(job Job) error {
	if store.validateJobInput(job.Prompt, job.Name, job.Schedule, job.Origin, job.Repeat.Times) != nil {
		return ErrInvalidJob
	}
	if job.CreatedAt.IsZero() || job.Repeat.Completed < 0 || job.Repeat.Completed > 1_000_000 || len(job.LastError) > store.options.MaxErrorBytes {
		return ErrInvalidJob
	}
	if job.Repeat.Times > 0 && job.Repeat.Completed > job.Repeat.Times {
		return ErrInvalidJob
	}
	if job.Enabled && job.NextRunAt.IsZero() {
		return ErrInvalidJob
	}
	if job.LastStatus != "" && job.LastStatus != StatusOK && job.LastStatus != StatusError {
		return ErrInvalidJob
	}
	return nil
}

func (store *Store) writeLocked(jobs []Job) error {
	if err := store.ensureDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(diskFile{Version: 1, Jobs: jobs}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cron jobs: %w", err)
	}
	data = append(data, '\n')
	if int64(len(data)) > store.options.MaxFileBytes {
		return ErrStoreBound
	}
	temporary, err := os.CreateTemp(store.dir, ".jobs-*.tmp")
	if err != nil {
		return fmt.Errorf("create cron jobs temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure cron jobs temporary file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write cron jobs: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync cron jobs: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close cron jobs: %w", err)
	}
	if err := replaceFile(temporaryName, store.path); err != nil {
		return fmt.Errorf("replace cron jobs: %w", err)
	}
	if err := os.Chmod(store.path, 0o600); err != nil {
		return fmt.Errorf("secure cron jobs: %w", err)
	}
	return syncDirectory(store.dir)
}

func (store *Store) ensureDir() error {
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		return fmt.Errorf("create cron directory: %w", err)
	}
	return os.Chmod(store.dir, 0o700)
}

func safeID(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("_.-", character)) {
			return false
		}
	}
	return true
}

func sameOrigin(left, right Origin) bool {
	return left.ConversationID == right.ConversationID && left.ChannelID == right.ChannelID
}

func utcNow(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func randomID() (string, error) {
	data := make([]byte, 6)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
