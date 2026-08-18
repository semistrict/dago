package cron

import (
	"context"
	"path/filepath"
	"testing"
)

func TestConstructorsRejectNegativeLimitsAndKeepZeroDefaultsUseful(t *testing.T) {
	store := NewStore("assistant", filepath.Join(t.TempDir(), "cron"), Options{})
	if store.options.MaxJobs != defaultMaxJobs || store.options.MaxFileBytes != defaultMaxFileBytes || store.options.MaxPromptBytes != defaultMaxPrompt {
		t.Fatalf("store defaults = %+v", store.options)
	}
	scheduler := NewScheduler(store, func(context.Context, Job, string) error { return nil }, SchedulerOptions{})
	if scheduler.options.TickInterval <= 0 || scheduler.options.JobTimeout <= 0 || scheduler.options.MaxDuePerTick <= 0 || scheduler.options.Now == nil || scheduler.options.Logger == nil {
		t.Fatalf("scheduler defaults = %+v", scheduler.options)
	}
	for name, call := range map[string]func(){
		"store": func() { NewStore("assistant", t.TempDir(), Options{MaxJobs: -1}) },
		"scheduler": func() {
			NewScheduler(store, func(context.Context, Job, string) error { return nil }, SchedulerOptions{JobTimeout: -1})
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("negative static limit did not panic")
				}
			}()
			call()
		})
	}
}
