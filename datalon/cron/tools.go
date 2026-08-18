package cron

import (
	"context"
	"strings"
	"time"

	"github.com/semistrict/dago/datool"
)

// OriginFactory returns the current channel conversation for a tool call.
type OriginFactory func(context.Context) (Origin, error)

// Tools returns conversation-scoped create, list, edit, and remove tools. Store
// and origin are required positional dependencies; static nil values panic.
func Tools(store *Store, origin OriginFactory) []datool.Tool {
	if store == nil {
		panic("datalon/cron: nil store")
	}
	if origin == nil {
		panic("datalon/cron: nil origin factory")
	}
	type createInput struct {
		Prompt      string `json:"prompt" description:"Self-contained prompt to run when the job fires."`
		Schedule    string `json:"schedule" description:"Schedule such as in 30m or every 15m."`
		Name        string `json:"name,omitempty" description:"Optional human-readable job name."`
		RepeatTimes *int   `json:"repeat_times,omitempty" description:"Optional recurring attempt cap."`
	}
	create := datool.MustNew("create_job", "Schedule a background task that will later deliver to this conversation.", func(ctx context.Context, input createInput) (JobView, error) {
		scope, err := origin(ctx)
		if err != nil {
			return JobView{}, err
		}
		schedule, err := ParseSchedule(input.Schedule)
		if err != nil {
			return JobView{}, err
		}
		repeat := 0
		if input.RepeatTimes != nil {
			repeat = *input.RepeatTimes
		}
		job, err := store.Create(ctx, input.Prompt, schedule, scope, CreateOptions{Name: input.Name, RepeatTimes: repeat})
		return view(job), err
	})

	list := datool.MustNew("list_jobs", "List scheduled jobs created from this conversation.", func(ctx context.Context, _ struct{}) ([]JobView, error) {
		scope, err := origin(ctx)
		if err != nil {
			return nil, err
		}
		jobs, err := store.List(ctx, &scope)
		if err != nil {
			return nil, err
		}
		result := make([]JobView, len(jobs))
		for index, job := range jobs {
			result[index] = view(job)
		}
		return result, nil
	})

	type editInput struct {
		JobID       string  `json:"job_id" description:"Job ID returned by create or list."`
		Name        *string `json:"name,omitempty" description:"Replacement job name."`
		Prompt      *string `json:"prompt,omitempty" description:"Replacement prompt."`
		Schedule    *string `json:"schedule,omitempty" description:"Replacement schedule."`
		Enabled     *bool   `json:"enabled,omitempty" description:"Pause or resume the job."`
		RepeatTimes *int    `json:"repeat_times,omitempty" description:"Replacement recurring cap; zero means unlimited."`
	}
	edit := datool.MustNew("edit_job", "Update a scheduled job from this conversation.", func(ctx context.Context, input editInput) (JobView, error) {
		scope, err := origin(ctx)
		if err != nil {
			return JobView{}, err
		}
		options := EditOptions{Name: input.Name, Prompt: input.Prompt, Enabled: input.Enabled, RepeatTimes: input.RepeatTimes}
		if input.Schedule != nil {
			schedule, parseErr := ParseSchedule(*input.Schedule)
			if parseErr != nil {
				return JobView{}, parseErr
			}
			options.Schedule = &schedule
		}
		job, err := store.Edit(ctx, stripQuotes(input.JobID), scope, options)
		return view(job), err
	})

	type removeInput struct {
		JobID string `json:"job_id" description:"Job ID returned by create or list."`
	}
	remove := datool.MustNew("remove_job", "Delete a scheduled job from this conversation.", func(ctx context.Context, input removeInput) (JobView, error) {
		scope, err := origin(ctx)
		if err != nil {
			return JobView{}, err
		}
		job, err := store.Remove(ctx, stripQuotes(input.JobID), scope)
		return view(job), err
	})
	return []datool.Tool{create, list, edit, remove}
}

// JobView is the model-visible, conversation-scoped job representation.
type JobView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Prompt     string    `json:"prompt"`
	Schedule   Schedule  `json:"schedule"`
	Repeat     Repeat    `json:"repeat"`
	Enabled    bool      `json:"enabled"`
	NextRunAt  time.Time `json:"next_run_at,omitzero"`
	LastRunAt  time.Time `json:"last_run_at,omitzero"`
	LastStatus Status    `json:"last_status,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
}

func view(job Job) JobView {
	return JobView{
		ID: job.ID, Name: job.Name, Prompt: job.Prompt, Schedule: job.Schedule,
		Repeat: job.Repeat, Enabled: job.Enabled, NextRunAt: job.NextRunAt,
		LastRunAt: job.LastRunAt, LastStatus: job.LastStatus, LastError: job.LastError,
	}
}

func stripQuotes(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '\'' || value[0] == '"') {
		return strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}
