import { expect, test, type BrowserContext, type Page } from "@playwright/test";

async function terminalText(page: Page): Promise<string> {
  return page.evaluate(() => {
    const terminal = window.dacodeTerminal;
    if (!terminal) return "";
    const lines: string[] = [];
    for (let row = 0; row < terminal.buffer.active.length; row += 1) {
      lines.push(terminal.buffer.active.getLine(row)?.translateToString(true) ?? "");
    }
    return lines.join("\n");
  });
}

async function visibleText(page: Page): Promise<string> {
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

async function openTerminal(page: Page, url: string | undefined): Promise<void> {
  expect(url).toBeTruthy();
  await page.goto(url ?? "/");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(async () => (await visibleText(page)).trim().length).toBeGreaterThan(10);
}

async function reconnect(context: BrowserContext, page: Page, url: string | undefined): Promise<Page> {
  await page.close();
  const next = await context.newPage();
  await openTerminal(next, url);
  return next;
}

async function command(page: Page, value: string): Promise<void> {
  await page.keyboard.type(value);
  await page.keyboard.press("Enter");
}

async function selectRow(page: Page, label: string): Promise<void> {
  for (let index = 0; index < 100; index += 1) {
    const text = await visibleText(page);
    if (text.split("\n").some((line) => line.includes(label) && /[>›]/u.test(line))) return;
    await page.keyboard.press("ArrowDown");
    await page.waitForTimeout(20);
  }
  throw new Error(`could not select ${label}`);
}

async function openNotification(page: Page, title: string): Promise<void> {
  await page.keyboard.press("Control+n");
  await expect.poll(() => visibleText(page)).toContain("Notifications");
  await selectRow(page, title);
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleText(page)).toContain("(recommended)");
}

test("/auto-update persists disable and enable, honors exact environment overrides, and fails closed for unsafe preferences", async ({ context, page }) => {
  const url = process.env.PLAYWRIGHT_NOTIFY_URL;
  await openTerminal(page, url);
  await command(page, "/auto-update");
  await expect.poll(() => visibleText(page)).toContain("Automatic updates disabled.");

  page = await reconnect(context, page, url);
  await command(page, "/auto-update");
  await expect.poll(() => visibleText(page)).toContain("Automatic updates enabled.");

  const overridden = await context.newPage();
  await openTerminal(overridden, process.env.PLAYWRIGHT_AUTO_DISABLED_URL);
  await command(overridden, "/auto-update");
  await expect.poll(() => visibleText(overridden)).toContain("controlled by DEEPAGENTS_CODE_AUTO_UPDATE");

  const malformed = await context.newPage();
  await openTerminal(malformed, process.env.PLAYWRIGHT_AUTO_MALFORMED_URL);
  await command(malformed, "/auto-update");
  await malformed.waitForTimeout(300);
  expect(await visibleText(malformed)).not.toContain("Automatic updates enabled.");
  expect(await terminalText(malformed)).not.toContain("not-a-boolean");

  for (const unsafeURL of [process.env.PLAYWRIGHT_AUTO_SYMLINK_URL, process.env.PLAYWRIGHT_AUTO_UNWRITABLE_URL]) {
    const unsafe = await context.newPage();
    await openTerminal(unsafe, unsafeURL);
    await command(unsafe, "/auto-update");
    await unsafe.waitForTimeout(300);
    expect(await visibleText(unsafe)).not.toContain("Automatic updates enabled.");
    expect(await terminalText(unsafe)).not.toContain("fixture-preferences-target");
  }
});

test("first implicit default only notifies, next launch applies once, failures cool down, and replacement is safe", async ({ context, page }) => {
  const url = process.env.PLAYWRIGHT_UPDATE_AVAILABLE_URL;
  await openTerminal(page, url);
  await expect.poll(() => visibleText(page)).toContain("Update v9.9.9 available");
  expect(await visibleText(page)).not.toContain("Update installed.");

  page = await reconnect(context, page, url);
  await expect.poll(() => visibleText(page), { timeout: 5_000 }).toContain("Update installed.");
  page = await reconnect(context, page, url);
  await expect.poll(() => visibleText(page)).toContain("restart this process to use v9.9.9");

  let failed = await context.newPage();
  const failedURL = process.env.PLAYWRIGHT_UPDATE_FAIL_URL;
  await openTerminal(failed, failedURL);
  failed = await reconnect(context, failed, failedURL);
  await failed.waitForTimeout(900);
  expect(await visibleText(failed)).not.toContain("Update installed.");
  failed = await reconnect(context, failed, failedURL);
  await failed.waitForTimeout(300);
  expect(await visibleText(failed)).not.toContain("Update v9.9.9 available");

  let windows = await context.newPage();
  const windowsURL = process.env.PLAYWRIGHT_UPDATE_WINDOWS_URL;
  await openTerminal(windows, windowsURL);
  windows = await reconnect(context, windows, windowsURL);
  await expect.poll(() => visibleText(windows)).toContain("Update v9.9.9 available");
  expect(await visibleText(windows)).not.toContain("Update installed.");
});

test("/notifications supports navigation, persistence, Escape, Ctrl+C, Ctrl+D, and rollback", async ({ context, page }) => {
  const url = process.env.PLAYWRIGHT_NOTIFY_SETTINGS_URL;
  await openTerminal(page, url);
  await command(page, "/notifications");
  await expect.poll(() => visibleText(page)).toContain("Notification Settings");
  await page.keyboard.press("Space");
  await expect.poll(() => visibleText(page)).toContain("Notification preference saved.");
  await page.keyboard.press("Escape");
  await expect.poll(() => visibleText(page)).not.toContain("Notification Settings");

  page = await reconnect(context, page, url);
  await command(page, "/notifications");
  await expect.poll(() => visibleText(page)).toContain("[ ] Warn when ripgrep is not installed");
  await page.keyboard.press("Control+c");
  await expect.poll(() => visibleText(page)).not.toContain("Notification Settings");
  await command(page, "/notifications");
  await page.keyboard.press("Control+d");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "closed");

  const rollback = await context.newPage();
  await openTerminal(rollback, process.env.PLAYWRIGHT_NOTIFY_FAIL_URL);
  await command(rollback, "/notifications");
  await rollback.keyboard.press("Space");
  await expect.poll(() => visibleText(rollback)).toContain("Notification preference could not be saved.");
  await expect.poll(() => visibleText(rollback)).toContain("[x] Warn when ripgrep is not installed");
});

test("generic and actionable toasts route through Ctrl+N without stealing model-selector Ctrl+N", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_NOTIFY_LAYOUT_URL);
  const initial = await visibleText(page);
  expect(initial).toContain("Generic fixture notice");
  expect(initial).toContain("Unrestricted mode is active");

  await page.waitForTimeout(100);
  await page.keyboard.press("Control+n");
  await expect.poll(() => visibleText(page)).toContain("Notifications");
  const center = await visibleText(page);
  expect(center).toContain("Notifications");
  expect(center).toContain("ripgrep is not installed");
  expect(center).toContain("Web search is not configured");
  expect(center).toContain("Generic fixture notice");
  await page.keyboard.press("Escape");
  await command(page, "/model");
  await expect.poll(() => visibleText(page)).toContain("Select Model");
  await page.keyboard.press("Control+n");
  await expect.poll(() => visibleText(page)).toContain("Select Model");
});

test("notification actions copy without executing, open validated HTTPS, stack API-key auth, and retain failed suppression", async ({ context, page }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const url = process.env.PLAYWRIGHT_NOTIFY_ACTIONS_URL;
  await openTerminal(page, url);
  await openNotification(page, "ripgrep is not installed");
  await page.keyboard.press("Enter");
  await expect(page.locator("html")).toHaveAttribute("data-clipboard-state", "copied");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("echo notification-copy-sentinel");
  expect(await terminalText(page)).not.toContain("Offline fixture response.");

  await openNotification(page, "ripgrep is not installed");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect(page.locator("html")).toHaveAttribute("data-open-url-state", "opened");
  await expect(page.locator("html")).toHaveAttribute("data-opened-url", "https://github.com/BurntSushi/ripgrep");

  await openNotification(page, "Web search is not configured");
  await page.waitForTimeout(100);
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleText(page)).toContain("Add API key");
  await page.keyboard.type("fixture-browser-secret");
  await expect.poll(() => visibleText(page)).toContain("API key: <hidden>");
  expect(await terminalText(page)).not.toContain("fixture-browser-secret");
  await page.keyboard.press("Escape");

  const failed = await context.newPage();
  await openTerminal(failed, process.env.PLAYWRIGHT_NOTIFY_FAIL_URL);
  await openNotification(failed, "Unrestricted mode is active");
  await failed.keyboard.press("ArrowDown");
  await failed.keyboard.press("Enter");
  await expect.poll(() => visibleText(failed)).toContain("Notification preference could not be saved.");
  await page.keyboard.press("Control+n");
  await expect.poll(() => visibleText(failed)).toContain("Unrestricted mode is active");
});

test("/trace opens the exact URL, explains empty traces, defers busy transcript text, and sanitizes failures", async ({ context, page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_TRACE_URL);
  await command(page, "/trace");
  const exactURL = "https://github.com/semistrict/dago/actions?project=fixture-project&thread=fixture-thread";
  await expect(page.locator("html")).toHaveAttribute("data-opened-url", exactURL);
  await expect.poll(() => terminalText(page)).toContain("The trace will be empty until you send the first message");

  await command(page, "slow fixture message");
  await command(page, "/trace");
  await expect(page.locator("html")).toHaveAttribute("data-opened-url", exactURL);
  await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("Offline fixture response.");
  const after = await terminalText(page);
  expect(after).toContain("/trace");

  const failed = await context.newPage();
  await openTerminal(failed, process.env.PLAYWRIGHT_TRACE_FAIL_URL);
  await command(failed, "/trace");
  await expect.poll(() => terminalText(failed)).toContain("Failed to resolve LangSmith thread URL.");
  expect(await terminalText(failed)).not.toContain("fixture secret trace failure");
  await expect(failed.locator("html")).not.toHaveAttribute("data-open-url-state", "opened");

  const unconfigured = await context.newPage();
  await openTerminal(unconfigured, process.env.PLAYWRIGHT_TRACE_UNCONFIGURED_URL);
  await command(unconfigured, "/trace");
  await expect.poll(() => terminalText(unconfigured)).toContain("LangSmith tracing is not configured");
  await expect(unconfigured.locator("html")).not.toHaveAttribute("data-open-url-state", "opened");

  const timedOut = await context.newPage();
  await openTerminal(timedOut, process.env.PLAYWRIGHT_TRACE_TIMEOUT_URL);
  await command(timedOut, "/trace");
  await expect.poll(() => terminalText(timedOut), { timeout: 5_000 }).toContain("Could not reach LangSmith");
  await expect(timedOut.locator("html")).not.toHaveAttribute("data-open-url-state", "opened");

  const unsafe = await context.newPage();
  await openTerminal(unsafe, process.env.PLAYWRIGHT_TRACE_UNSAFE_URL);
  await command(unsafe, "/trace");
  await expect.poll(() => terminalText(unsafe)).toContain("Failed to resolve LangSmith thread URL.");
  expect(await terminalText(unsafe)).not.toContain("fixture-secret");
  await expect(unsafe.locator("html")).not.toHaveAttribute("data-open-url-state", "opened");
});

test("/update shows current state and requires a two-step confirmation before one apply", async ({ context, page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_UPDATE_CURRENT_URL);
  await command(page, "/update");
  await expect.poll(() => visibleText(page)).toContain("You are up to date.");
  await page.keyboard.press("Escape");
  await expect.poll(() => visibleText(page)).not.toContain("Software Update");

  const available = await context.newPage();
  await openTerminal(available, process.env.PLAYWRIGHT_UPDATE_AVAILABLE_UI_URL);
  await command(available, "/update");
  await expect.poll(() => visibleText(available)).toContain("Update available");
  await available.keyboard.press("Enter");
  await expect.poll(() => visibleText(available)).toContain("Replace the current executable?");
  await available.keyboard.press("Escape");
  await expect.poll(() => visibleText(available)).toContain("Update available");
  await available.keyboard.press("Enter");
  await available.keyboard.press("Enter");
  await expect.poll(() => visibleText(available), { timeout: 5_000 }).toContain("Update installed.");
});

test("update checking and apply cancellation ignore stale work but surface already-completed activation", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_UPDATE_SLOW_URL);
  await command(page, "/update");
  await expect.poll(() => visibleText(page)).toContain("Update available");
  await page.keyboard.press("Enter");
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleText(page)).toContain("Downloading and verifying update");
  await page.keyboard.press("Escape");
  await expect.poll(() => visibleText(page), { timeout: 5_000 }).toContain("Update installed.");
});

test("safe update failures retry and concurrent sessions permit only one apply", async ({ context, page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_UPDATE_RETRY_URL);
  await command(page, "/update");
  await expect.poll(() => visibleText(page)).toContain("release channel could not be checked");
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleText(page)).toContain("Update available");

  const first = await context.newPage();
  const second = await context.newPage();
  await openTerminal(first, process.env.PLAYWRIGHT_UPDATE_SHARED_URL);
  await openTerminal(second, process.env.PLAYWRIGHT_UPDATE_SHARED_URL);
  for (const candidate of [first, second]) {
    await command(candidate, "/update");
    await expect.poll(() => visibleText(candidate)).toContain("Update available");
    await candidate.keyboard.press("Enter");
    await candidate.keyboard.press("Enter");
  }
  await expect.poll(async () => `${await visibleText(first)}\n${await visibleText(second)}`, { timeout: 5_000 }).toContain("Another update is already running");
  await expect.poll(async () => `${await visibleText(first)}\n${await visibleText(second)}`, { timeout: 5_000 }).toContain("Update installed.");
});

test("startup update notification defaults to Install and supports changelog, remind, skip, and restart persistence", async ({ context, page }) => {
  const url = process.env.PLAYWRIGHT_UPDATE_STARTUP_CHOICE_URL;
  await openTerminal(page, url);
  await expect.poll(() => visibleText(page)).toContain("Update v9.9.9 available");
  await expect.poll(() => visibleText(page)).toContain("Install (recommended)");
  await selectRow(page, "Open changelog");
  await page.waitForTimeout(50);
  await page.keyboard.press("Enter");
  await expect(page.locator("html")).toHaveAttribute("data-opened-url", "https://github.com/semistrict/dago/releases");
  await expect.poll(() => visibleText(page)).toContain("Update v9.9.9 available");

  await selectRow(page, "Remind me later");
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleText(page)).toContain("Update reminder deferred until the next launch.");
  page = await reconnect(context, page, url);
  await expect.poll(() => visibleText(page)).toContain("Update v9.9.9 is available.");
  await expect.poll(() => visibleText(page)).toContain("Update v9.9.9 available");
  await selectRow(page, "Skip this version");
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleText(page)).toContain("This update version will be skipped.");
  page = await reconnect(context, page, url);
  await expect.poll(() => visibleText(page)).not.toContain("Update v9.9.9 is available.");
});

test("narrow ASCII rendering bounds three toasts and a modal while keeping composer and status visible", async ({ page }) => {
  await page.setViewportSize({ width: 560, height: 420 });
  await openTerminal(page, process.env.PLAYWRIGHT_NOTIFY_LAYOUT_URL);
  await page.keyboard.press("Control+n");
  await expect.poll(() => visibleText(page)).toContain("Notifications");
  const snapshot = await page.evaluate(() => {
    const terminal = window.dacodeTerminal!;
    const lines: string[] = [];
    for (let row = terminal.buffer.active.viewportY; row < terminal.buffer.active.viewportY + terminal.rows; row += 1) {
      lines.push(terminal.buffer.active.getLine(row)?.translateToString(true) ?? "");
    }
    return { cols: terminal.cols, rows: terminal.rows, lines };
  });
  expect(snapshot.lines).toHaveLength(snapshot.rows);
  expect(snapshot.lines.every((line) => Array.from(line).length <= snapshot.cols)).toBe(true);
  const rendered = snapshot.lines.join("\n");
  expect(rendered).toContain("Notifications");
	expect(rendered).toMatch(/^auto\s/m);
  expect(rendered).toContain(">");
  expect(rendered).not.toMatch(/[─│┌┐└┘•…✓✗⏳↑↓]/u);
});
