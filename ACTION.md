# dago Code action

The repository-root `action.yml` runs `dacode` headlessly in a GitHub Actions
job. It builds the agent from the action revision, so pin the action to a commit
SHA in production workflows.

```yaml
permissions:
  contents: read

steps:
  - uses: actions/checkout@d23441a48e516b6c34aea4fa41551a30e30af803
  - uses: semistrict/dago@0123456789abcdef0123456789abcdef01234567
    id: agent
    with:
      prompt: "Review this checkout and fix the failing tests."
      openai_api_key: ${{ secrets.OPENAI_API_KEY }}
```

`prompt` and `openai_api_key` are required inputs. The useful unattended defaults
are a 30-minute timeout, 50 agentic turns, final-response-only output, automatic
model review for gated operations, and the `recommended,git,gh` shell allow-list.
The action never enables unrestricted local execution by default. The optional
`model`, `approval_model`, `working_directory`, `max_turns`, `timeout`, `quiet`,
and `shell_allow_list` inputs map directly to supported CLI behavior.

The `response`, `exit_code`, and `cache_hit` outputs report the agent result. A
nonzero agent result fails the action after its memory has been saved. Output is
written with an unpredictable multiline delimiter so agent text cannot create
forged workflow outputs.

## Durable memory

Memory is enabled by default. The action restores and saves only the thread
database files with `actions/cache`—credentials and display settings are
excluded—then resumes a stable thread derived from `agent_name` and
`memory_scope`. Choose `pr`, `branch`, or `repo` scope, use a different
`agent_name` to isolate identities, or set `enable_memory: "false"` for an
ephemeral run. The default scope is `branch`. Cache keys include the runner
platform and a unique run suffix; restore keys only search within the selected
identity and scope.

## Skills repositories

Set `skills_repo` to `owner/repository`, `owner/repository@ref`, or an HTTPS
GitHub repository URL. Private repositories use `github_token`. Each discovered
skill directory must contain `SKILL.md`, have a safe unique name, contain no
symbolic links, and not replace an existing workspace skill. Installed skills
are copied to `.deepagents/skills` before the agent starts.

The action accepts only GitHub-hosted repositories. This prevents an untrusted
input from sending the workflow token to an arbitrary Git server. Pin a skills
repository ref to an immutable commit when its contents are not maintained in
the workflow repository.
