---
type: Engineering Workflow
title: Evaluation, CI, and packaged delivery
description: How dago separates deterministic behavioral evaluation, benchmark adapters, CI gates, and packaged automation.
tags: [evaluations, harbor, ci, action, delivery]
---

# Evaluation, CI, and packaged delivery

## Evaluation layers

`daeval` is provider-neutral. A caller supplies the runnable agent or benchmark runner;
the library captures bounded trajectories, applies correctness checks and soft
efficiency expectations, and emits deterministic versioned reports. Runtime failures
remain distinct from behavioral failures so infrastructure does not masquerade as
quality.

`daeval/harbor` adapts benchmark tasks and results without creating sandboxes, loading
credentials, or contacting registries. The supplied runner owns isolation, images,
network policy, cleanup, and verifier integrity. ContextBench and DRBench adapters
validate and normalize task inputs without exposing gold answers or verifier secrets to
the agent. `daeval/clbench` provides the continual-learning system boundary. See
`docs/EVALUATIONS.md` for report schemas, limits, and integration examples.

Cross-model scorecards must retain model, harness, benchmark/category, trial, and any
variant dimensions. Missing or incomplete leaves stay explicit; aggregation must not
silently rank partial data. Deterministic unit tests validate orchestration and report
math, but they are not evidence that a paid live model or remote sandbox performed well.

## CI gates

`.github/workflows/ci.yml` runs `make check` on Linux and macOS, validates the TinyGo
closure, verifies the separate Shelley Go/UI modules, runs browser/WASM coverage, and
performs a verified-secret scan. Third-party actions are pinned to immutable commits.
Network-bearing live model checks are deliberately not part of the normal deterministic
gate.

## Packaged automation

The root action builds the terminal agent from its pinned action revision, requires the
task and credential explicitly, installs only validated skill repositories, applies
bounded headless defaults, and caches only thread database files. `ACTION.md` is the
operator contract. Changes to `action.yml` and `scripts/github-action/` need the offline
tests in `internal/githubactiontest` plus shell syntax checks.

The scheduled wiki workflow runs only on the trusted default branch schedule or manual
dispatch, uses pinned actions and a pinned OpenWiki package version, grants write access
only to its update job, and limits the generated pull request to `openwiki/`. It is a
documentation proposal path, not an authority to change source code.

## Change checklist

- Keep runner/model/provider dependencies injected; normal evaluation tests must be network-free.
- Bound tasks, trials, trajectory size, output, failure details, time, and concurrency.
- Preserve cancellation and panic containment without converting infrastructure errors into failed quality checks.
- Keep report JSON versioned and deterministic; sort dimensions and map-derived output.
- Document credentials, cost, remote isolation, and verifier trust for any live wrapper.
- Run package tests/race/vet, `go mod tidy -diff`, `git diff --check`, and then `make check`.
