# TUI development

Every new or changed user-facing TUI behavior must include Playwright coverage in
`xtermjs/e2e`. Go unit tests complement this coverage but do not replace it.

Run `pnpm test:e2e` from `xtermjs` after changing TUI behavior.
