# Assistant-state lifecycle

`datalon/lifecycle` applies the pinned sensitive-state retention policy to one
explicit per-assistant state directory. The state root and a `CronStore` are
required positional dependencies; `New` performs no I/O.

```go
stateDir := config.StateDir()
cronStore := cron.NewStoreForConfig(config, cron.Options{})
retention := lifecycle.New(stateDir, cronStore, lifecycle.Options{})

preview, err := retention.DryRun(ctx)
report, err := retention.Clean(ctx)
```

Zero options retain completed cron jobs for 30 days and inbound files beneath
`media/inbound` for 24 hours, matching the pinned defaults. One artifact is
limited to 1 GiB, the default global Talon media ceiling; selected bytes, cron
records, filesystem entries, depth, policies, and audit records have separate
finite bounds. `ImmediateCronCleanup` and `ImmediateMediaCleanup` represent the
upstream zero-retention setting without making the Go options zero value
destructive. `OptionsFromEnv` parses the three pinned environment variables and
returns errors for malformed external values; the static constructor remains
error-free.

The dry-run and cleanup reports contain counts, byte totals, timestamps, kinds,
and stable SHA-256-derived references. They never include cron prompts, raw job
IDs, filenames, absolute paths, file content, session tokens, or trace content.
Cleanup first validates the entire bounded tree. A symlink, special file,
escaping policy, unexpected replacement, or exceeded limit aborts before file
deletion. Completed cron removal uses the store's locked, validated, atomic
`jobs.json` replacement. Regular files use atomic unlink after an identity
recheck; empty directories are removed only after retained files are handled.

The state root and managed artifact directories are secured to mode 0700 and
regular artifact files to mode 0600 during cleanup. Dry-run does not delete or
chmod files, although a concrete cron store may perform its normal directory
validation while listing jobs.

## Channel sessions and tracing

The pinned lifecycle does not delete durable channel credentials or remotely
hosted LangSmith traces. Go keeps those safe preservation defaults. Applications
that persist disposable local channel artifacts or trace exports can add a
confined `FilePolicy` below `channels/<provider>/artifacts`, `traces`, or
`tracing`. Deleting a channel
session can log out a provider or destroy pairing state, so `ArtifactSession`
requires the exact `SessionDeletionAcknowledgement` value. Run cleanup before
starting channel and scheduler background work; the manager serializes its own
runs, but cannot coordinate with arbitrary writers outside the supplied cron
store.

Cancellation is checked before walking, cron replacement, each security update,
each unlink, and each empty-directory removal. A cancellation or filesystem
failure can leave an already-completed atomic cron replacement or unlink in
place; the returned report marks only completed audited operations and is safe
to retain in operator logs.
