import { expect, test, type Page } from "@playwright/test";

async function terminalText(page: Page): Promise<string> {
  return page.evaluate(() => {
    const terminal = window.dacodeTerminal;
    if (!terminal) return "";
    const buffer = terminal.buffer.active;
    const lines: string[] = [];
    for (let index = 0; index < buffer.length; index += 1) {
      lines.push(buffer.getLine(index)?.translateToString(true) ?? "");
    }
    return lines.join("\n");
  });
}

async function visibleTerminalText(page: Page): Promise<string> {
  return page.evaluate(() => {
    const terminal = window.dacodeTerminal;
    if (!terminal) return "";
    const buffer = terminal.buffer.active;
    const lines: string[] = [];
    for (let row = buffer.viewportY; row < buffer.viewportY + terminal.rows; row += 1) {
      lines.push(buffer.getLine(row)?.translateToString(true) ?? "");
    }
    return lines.join("\n");
  });
}

async function openTerminal(page: Page, url = process.env.PLAYWRIGHT_INTERACTIONS_URL, readyText = "Ready to code"): Promise<void> {
  expect(url).toBeTruthy();
  await page.goto(url ?? "/");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain(readyText);
}

async function runCommand(page: Page, command: string): Promise<void> {
  await page.keyboard.type(command, { delay: 15 });
  await page.keyboard.press("Enter");
}

async function openThreadList(page: Page, query = ""): Promise<void> {
  await runCommand(page, "/threads");
  await expect.poll(() => visibleTerminalText(page)).toContain("Threads");
  if (query) {
    await page.keyboard.type(query, { delay: 15 });
    await expect.poll(() => visibleTerminalText(page)).toContain(`Search: ${query}`);
  }
}

async function focusThreadList(page: Page): Promise<void> {
  await page.keyboard.press("Tab");
  await page.keyboard.press("Tab");
}

test("/install confirms allowlisted entries, cancels safely, and --force reports restart", async ({ page }) => {
  await openTerminal(page);
  await runCommand(page, "/install external-helper");
  await expect.poll(() => visibleTerminalText(page)).toContain("external-helper");
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleTerminalText(page)).toContain("Run the allowlisted installer for external-helper?");
  await page.keyboard.press("Escape");
  await expect.poll(() => visibleTerminalText(page)).toContain("Install optional integration");
  expect(await terminalText(page)).not.toContain("Restart required to load it");
  await page.keyboard.press("Escape");

  await runCommand(page, "/install external-helper --force");
  await expect.poll(() => visibleTerminalText(page)).toContain("confirmation will be skipped");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Installed external-helper. Restart required to load it.");
});

test("startup failure leaves the allowlisted install recovery path usable", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_INTERACTIONS_STARTUP_FAILED_URL, "Startup failed");
  await expect.poll(() => terminalText(page)).toContain("Fixture startup failed; recovery commands remain available.");
  await runCommand(page, "/install external-helper --force");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Installed external-helper. Restart required to load it.");
});

test("a busy model selection is deferred and applied before the next queued prompt", async ({ page }) => {
  await openTerminal(page);
  await runCommand(page, "slow model deferral");
  await expect.poll(() => terminalText(page)).toContain("Thinking");
  await runCommand(page, "/model");
  await expect.poll(() => visibleTerminalText(page)).toContain("Select Model");
  await page.keyboard.type("openai:gpt-5.6-sol", { delay: 15 });
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Model change queued for the next idle point.");
  await runCommand(page, "after deferred model");
  await expect.poll(() => terminalText(page)).toContain("Queued input #1.");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain(
    "model=openai:gpt-5.6-sol prompt=after deferred model",
  );
});

test("model availability is explicit and a failed default write rolls back", async ({ page }) => {
  await openTerminal(page);
  await runCommand(page, "/model");
  await expect.poll(() => visibleTerminalText(page)).toContain("[available]");
  await page.keyboard.press("Control+s");
  await expect.poll(() => terminalText(page)).toContain("Default model set to openai:gpt-5.6-terra.");
  await page.keyboard.press("Escape");

  await runCommand(page, "/model");
  await page.keyboard.type("openai:gpt-5.6-luna", { delay: 15 });
  await page.keyboard.press("Control+s");
  await expect.poll(() => terminalText(page)).toContain("Model preference could not be saved.");
  expect(await visibleTerminalText(page)).not.toContain("GPT-5.6 Luna (default)");
  await page.keyboard.press("Control+w");
  await expect.poll(() => visibleTerminalText(page)).toContain("GPT-5.6 Terra (current, default)");
  expect(await visibleTerminalText(page)).not.toContain("GPT-5.6 Luna (default)");
  await page.keyboard.press("Escape");

  await runCommand(page, "/model");
  await page.keyboard.type("anthropic:claude-opus-5", { delay: 15 });
  await expect.poll(() => visibleTerminalText(page)).toContain("install required; credentials required");
  await page.keyboard.press("Escape");

  await runCommand(page, "/model");
  await page.keyboard.type("openai:gpt-5.6-sol", { delay: 15 });
  await page.keyboard.press("Control+s");
  await page.keyboard.press("Control+w");
  await page.keyboard.type("openai:gpt-5.6-terra", { delay: 15 });
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Model set to openai:gpt-5.6-terra.");

  await runCommand(page, "/model");
  await page.keyboard.type("openai:gpt-5.6-terra", { delay: 15 });
  await page.keyboard.press("Control+s");
  await expect.poll(() => terminalText(page)).toContain("Default model set to openai:gpt-5.6-terra.");
  await page.keyboard.press("Control+w");
  await page.waitForTimeout(3_500);
  const afterStaleCompletion = await visibleTerminalText(page);
  expect(afterStaleCompletion).toContain("GPT-5.6 Terra (openai) (current, default)");
  expect(await terminalText(page)).not.toContain("Default model set to openai:gpt-5.6-sol.");
  await page.keyboard.type("openai:gpt-5.6-sol", { delay: 15 });
  await expect.poll(() => visibleTerminalText(page)).toContain("GPT-5.6 Sol [available]");
  expect(await visibleTerminalText(page)).not.toContain("GPT-5.6 Sol (default)");
  await page.keyboard.press("Escape");
  await runCommand(page, "report fixture model default");
  await expect.poll(() => terminalText(page)).toContain("persisted-default=openai:gpt-5.6-terra");
});

test("/threads searches, filters by agent, toggles time, and resumes an exact row", async ({ page }) => {
  await openTerminal(page);
  await openThreadList(page, "thread-alpha");
  const filtered = await visibleTerminalText(page);
  expect(filtered).toContain("thread-alpha");
  expect(filtered).not.toContain("thread-blue");
  await page.keyboard.press("Tab");
  await page.keyboard.press("ArrowDown");
  await expect.poll(() => visibleTerminalText(page)).toContain("Agent: dacode");
  await page.keyboard.press("Control+r");
  await page.keyboard.press("Tab");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Restored history for thread-alpha.");
});

test("thread deletion is confirmation-bound, durable on success, and rolls back on failure", async ({ page }) => {
  await openTerminal(page);
  await openThreadList(page, "thread-alpha");
  await focusThreadList(page);
  await page.keyboard.press("Control+d");
  await expect.poll(() => visibleTerminalText(page)).toContain("Delete thread-alpha?");
  await page.keyboard.press("Escape");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await page.keyboard.press("Control+d");
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleTerminalText(page)).toContain("No matching threads.");
  await page.keyboard.press("Escape");
  await openThreadList(page, "thread-alpha");
  await expect.poll(() => visibleTerminalText(page)).toContain("No matching threads.");
  await page.keyboard.press("Escape");

  await openThreadList(page, "thread-fail");
  await focusThreadList(page);
  await page.keyboard.press("Control+d");
  await expect.poll(() => visibleTerminalText(page)).toContain("Delete thread-fail?");
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleTerminalText(page)).toContain("Delete failed: fixture durable delete refused");
  expect(await visibleTerminalText(page)).toContain("thread-fail");
});

test("direct resume and a busy selector resume load exact checkpoint history", async ({ page, context }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_INTERACTIONS_DIRECT_URL);
  await expect.poll(() => terminalText(page)).toContain("Restored history for thread-alpha.");

  const busy = await context.newPage();
  await openTerminal(busy);
  await runCommand(busy, "slow before thread switch");
  await openThreadList(busy, "thread-alpha");
  await focusThreadList(busy);
  await busy.keyboard.press("Enter");
  await expect.poll(() => terminalText(busy)).toContain("Thread switch queued for the next idle point.");
  await expect.poll(() => terminalText(busy), { timeout: 10_000 }).toContain("Restored history for thread-alpha.");
});

test("Escape requires a second press to clear and Ctrl+Z restores the bounded draft", async ({ page }) => {
  await openTerminal(page);
  await page.keyboard.type("restore this draft", { delay: 15 });
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Press Esc again to clear input.");
  expect(await terminalText(page)).toContain("> restore this draft");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Input cleared.");
  await page.keyboard.press("Control+z");
  await expect.poll(() => terminalText(page)).toContain("> restore this draft");
  await expect.poll(() => terminalText(page)).toContain("Input restored.");
});

test("Ctrl+C copies a draft, rapid presses arm quit, and active work cancels first", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await openTerminal(page, process.env.PLAYWRIGHT_TEST_BASE_URL);
  await page.keyboard.type("clipboard draft", { delay: 15 });
  await expect.poll(() => terminalText(page)).toContain("> clipboard draft");
  await page.waitForTimeout(100);
  await page.keyboard.press("Control+C");
  await expect(page.locator("html")).toHaveAttribute("data-clipboard-state", "copied");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("clipboard draft");
  expect(await terminalText(page)).toContain("> clipboard draft");
  await page.keyboard.press("Control+C");
  await expect.poll(() => terminalText(page)).toContain("Press Ctrl+C again to quit.");

  const cancel = await context.newPage();
  await openTerminal(cancel);
  await runCommand(cancel, "slow cancellation target");
  await expect.poll(() => terminalText(cancel)).toContain("Thinking");
  await cancel.keyboard.press("Control+C");
  await expect(cancel.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await cancel.waitForTimeout(1_000);
  expect(await terminalText(cancel)).not.toContain("prompt=slow cancellation target");
});

test("Ctrl+D forward-deletes multibyte input, prioritizes thread delete, and quits at end", async ({ page, context }) => {
  await openTerminal(page);
  await page.keyboard.type("aé界b", { delay: 15 });
  await page.keyboard.press("ArrowLeft");
  await page.keyboard.press("ArrowLeft");
  await page.keyboard.press("Control+d");
  await expect.poll(() => terminalText(page)).toContain("> aéb");

  const selector = await context.newPage();
  await openTerminal(selector);
  await openThreadList(selector, "thread-blue");
  await selector.keyboard.press("Tab");
  await selector.keyboard.press("Space");
  await expect.poll(() => visibleTerminalText(selector)).toContain("Agent: All agents");
  await selector.keyboard.press("Tab");
  await selector.keyboard.press("Control+d");
  await expect.poll(() => visibleTerminalText(selector)).toContain("Delete thread-blue?");
  await expect(selector.locator("html")).toHaveAttribute("data-terminal-state", "connected");

  await page.keyboard.press("End");
  await page.keyboard.press("Control+d");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "closed");
});

test("/clear preserves resumable checkpoint history and queued clear runs only after idle", async ({ page }) => {
  await openTerminal(page);
  await runCommand(page, "checkpoint history marker");
  await expect.poll(() => terminalText(page)).toContain("prompt=checkpoint history marker");
  await runCommand(page, "/clear");
  await expect.poll(() => terminalText(page)).toContain("Previous thread remains resumable with /threads -r fixture-current.");
  await runCommand(page, "/threads -r fixture-current");
  await expect.poll(() => terminalText(page)).toContain("checkpoint history marker");

  await runCommand(page, "slow queued clear");
  await expect.poll(() => terminalText(page)).toContain("Thinking");
  await runCommand(page, "/clear");
  await expect.poll(() => terminalText(page)).toContain("Queued input #1.");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).not.toContain("fixture reply: thread=fixture-current model=openai:gpt-5.6-terra prompt=slow queued clear");
});

test("/force-clear drops queues and ignores late callbacks from the prior generation", async ({ page }) => {
  await openTerminal(page);
  await runCommand(page, "late callback sentinel");
  await expect.poll(() => terminalText(page)).toContain("Thinking");
  await runCommand(page, "queued prompt must be dropped");
  await expect.poll(() => terminalText(page)).toContain("Queued input #1.");
  await runCommand(page, "/force-clear");
  await expect.poll(() => terminalText(page)).toContain("> What would you like to build?");
  await page.waitForTimeout(2_500);
  const afterClear = await terminalText(page);
  expect(afterClear).not.toContain("fixture reply: thread=fixture-current");
  expect(afterClear).not.toContain("prompt=queued prompt must be dropped");
  await runCommand(page, "after force clear");
  await expect.poll(() => terminalText(page)).toContain("prompt=after force clear");
  expect(await terminalText(page)).toContain("thread=fixture-new-");
  await runCommand(page, "/threads -r fixture-current");
  await expect.poll(() => terminalText(page)).toContain("Restored history for fixture-current.");
  const resumed = await terminalText(page);
  expect(resumed).toContain("current checkpoint history");
  expect(resumed).not.toContain("late callback sentinel");
  expect(resumed).not.toContain("queued prompt must be dropped");
});

test("malformed urgent commands queue while the bare /q alias exits immediately", async ({ page, context }) => {
  await openTerminal(page);
  await runCommand(page, "slow malformed urgent");
  await runCommand(page, "/quit now");
  await expect.poll(() => terminalText(page)).toContain("Queued input #1.");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Unknown command: /quit now");

  const urgent = await context.newPage();
  await openTerminal(urgent);
  await runCommand(urgent, "slow urgent quit");
  await runCommand(urgent, "/q");
  await expect(urgent.locator("html")).toHaveAttribute("data-terminal-state", "closed");
});
