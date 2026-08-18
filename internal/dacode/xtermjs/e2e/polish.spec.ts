import { expect, test, type Page } from "@playwright/test";

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

async function openTerminal(page: Page, url: string | undefined, expected = "fixture:polish-model"): Promise<void> {
  expect(url).toBeTruthy();
  await page.goto(url ?? "/");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain(expected);
}

async function runCommand(page: Page, value: string): Promise<void> {
  await page.keyboard.type(value, { delay: 12 });
  await page.keyboard.press("Enter");
}

async function terminalBackground(page: Page): Promise<string | undefined> {
  return page.evaluate(() => window.dacodeTerminal?.options.theme?.background?.toUpperCase());
}

async function visibleTerminalRow(page: Page, needle: string): Promise<number> {
  return page.evaluate((text) => {
    const terminal = window.dacodeTerminal;
    if (!terminal) return -1;
    const buffer = terminal.buffer.active;
    for (let row = buffer.viewportY; row < buffer.viewportY + terminal.rows; row += 1) {
      if ((buffer.getLine(row)?.translateToString(true) ?? "").includes(text)) return row - buffer.viewportY;
    }
    return -1;
  }, needle);
}

async function stableVisibleTerminalRow(page: Page, needle: string): Promise<number> {
  let previous = -2;
  for (let attempt = 0; attempt < 20; attempt += 1) {
    const current = await visibleTerminalRow(page, needle);
    if (current >= 0 && current === previous) return current;
    previous = current;
    await page.waitForTimeout(100);
  }
  return -1;
}

async function pageToWelcome(page: Page): Promise<void> {
  for (let index = 0; index < 12; index += 1) await page.keyboard.press("PageUp");
  await expect.poll(() => visibleText(page)).toContain("dacode development");
}

async function terminalTextPoint(page: Page, needle: string): Promise<{ x: number; y: number } | undefined> {
  return page.evaluate((text) => {
    const terminal = window.dacodeTerminal;
    const screen = document.querySelector<HTMLElement>(".xterm-screen");
    if (!terminal || !screen) return undefined;
    const buffer = terminal.buffer.active;
    for (let row = buffer.viewportY; row < buffer.viewportY + terminal.rows; row += 1) {
      const line = buffer.getLine(row)?.translateToString(true) ?? "";
      const column = line.indexOf(text);
      if (column < 0) continue;
      const bounds = screen.getBoundingClientRect();
      return {
        x: bounds.left + ((column + 1.5) * bounds.width) / terminal.cols,
        y: bounds.top + ((row - buffer.viewportY + 0.5) * bounds.height) / terminal.rows,
      };
    }
    return undefined;
  }, needle);
}

async function clickTerminalText(page: Page, needle: string): Promise<void> {
  const point = await terminalTextPoint(page, needle);
  expect(point).toBeDefined();
  await page.waitForTimeout(350);
  await page.mouse.click(point?.x ?? 0, point?.y ?? 0);
}

test("live two-line status covers modes, activity, hooks, metrics, and narrow ASCII", async ({ page }) => {
  await page.setViewportSize({ width: 1700, height: 900 });
  await openTerminal(page, process.env.PLAYWRIGHT_POLISH_URL);
  const initial = await visibleText(page);
  for (const expected of ["manual", "feature-polish", "rubric:active", "fixture:polish-model", "Cache: 95%", "Context: 65%", "$12.50"]) {
    expect(initial).toContain(expected);
  }
  await page.keyboard.type("!");
  await expect.poll(() => visibleText(page)).toContain("SHELL");
  await page.keyboard.press("Escape");
  await runCommand(page, "slow status turn");
  await expect.poll(() => visibleText(page)).toContain("Thinking");
  await runCommand(page, "queued status turn");
  await expect.poll(() => visibleText(page)).toContain("1 message queued");

  const hook = await page.context().newPage();
  await openTerminal(hook, process.env.PLAYWRIGHT_POLISH_HOOK_URL);
  expect(await visibleText(hook)).toContain("Checking workspace");

  const ascii = await page.context().newPage();
  await ascii.setViewportSize({ width: 620, height: 420 });
  await openTerminal(ascii, process.env.PLAYWRIGHT_POLISH_ASCII_URL);
  const asciiText = await visibleText(ascii);
  expect(asciiText).not.toMatch(/[╭╮╰╯│─…✓✗⏳•⏎█░]/);
  const overflow = await ascii.evaluate(() => {
    const terminal = window.dacodeTerminal;
    if (!terminal) return true;
    const buffer = terminal.buffer.active;
    for (let row = buffer.viewportY; row < buffer.viewportY + terminal.rows; row += 1) {
      if ((buffer.getLine(row)?.translateToString(true) ?? "").length > terminal.cols) return true;
    }
    return false;
  });
  expect(overflow).toBe(false);
});

test("welcome phases and exact click targets stay bounded", async ({ context, page }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await openTerminal(page, process.env.PLAYWRIGHT_POLISH_URL);
  await pageToWelcome(page);
  await clickTerminalText(page, "dacode development");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("development");
  await clickTerminalText(page, "Thread ID:");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("fixture-thread-full-0123456789");
  await clickTerminalText(page, "Project:");
  await expect(page.locator("html")).toHaveAttribute("data-opened-url", "https://example.test/projects/fixture");
  await clickTerminalText(page, "MCP:");
  await expect.poll(() => visibleText(page)).toContain("MCP Servers");

  const failed = await context.newPage();
  await openTerminal(failed, process.env.PLAYWRIGHT_POLISH_FAILURE_URL);
  await pageToWelcome(failed);
  expect(await visibleText(failed)).toContain("Startup failed");
  expect(await visibleText(failed)).toContain("could not start");

  const shielded = await context.newPage();
  await openTerminal(shielded, process.env.PLAYWRIGHT_POLISH_URL);
  await pageToWelcome(shielded);
  const staleVersionPoint = await terminalTextPoint(shielded, "dacode development");
  expect(staleVersionPoint).toBeDefined();
  await shielded.evaluate(() => navigator.clipboard.writeText("modal-shield-sentinel"));
  await runCommand(shielded, "/model ");
  await expect.poll(() => visibleText(shielded)).toContain("Select Model");
  await shielded.mouse.click(staleVersionPoint?.x ?? 0, staleVersionPoint?.y ?? 0);
  await expect.poll(() => visibleText(shielded)).toContain("Select Model");
  await expect.poll(() => shielded.evaluate(() => navigator.clipboard.readText())).toBe("modal-shield-sentinel");
});

test("tips are fresh or fallback only and first submission dismisses them", async ({ context, page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_POLISH_URL);
  expect(await visibleText(page)).toContain("Tip:");
  await runCommand(page, "dismiss the startup tip");
  await expect.poll(() => visibleText(page)).not.toContain("Tip:");

  const resumed = await context.newPage();
  await openTerminal(resumed, process.env.PLAYWRIGHT_POLISH_RESUMED_URL);
  expect(await visibleText(resumed)).not.toContain("Tip:");

  const fallback = await context.newPage();
  await openTerminal(fallback, process.env.PLAYWRIGHT_POLISH_FALLBACK_URL);
  expect(await visibleText(fallback)).toContain("Tip: Use /help to see available commands");

  const goal = await context.newPage();
  await openTerminal(goal, process.env.PLAYWRIGHT_POLISH_GOAL_URL);
  await expect.poll(() => terminalText(goal)).not.toContain("Tip:");

  const prompt = await context.newPage();
  await openTerminal(prompt, process.env.PLAYWRIGHT_POLISH_PROMPT_URL);
  await expect.poll(() => terminalText(prompt)).toContain("fixture initial prompt");
  await expect.poll(() => terminalText(prompt)).not.toContain("Tip:");

  const shell = await context.newPage();
  await openTerminal(shell, process.env.PLAYWRIGHT_POLISH_URL);
  expect(await visibleText(shell)).toContain("Tip:");
  await runCommand(shell, "!true");
  await expect.poll(() => visibleText(shell)).not.toContain("Tip:");

  const slash = await context.newPage();
  await openTerminal(slash, process.env.PLAYWRIGHT_POLISH_URL);
  expect(await visibleText(slash)).toContain("Tip:");
  await runCommand(slash, "/help ");
  await expect.poll(() => visibleText(slash)).not.toContain("Tip:");

  const queued = await context.newPage();
  await openTerminal(queued, process.env.PLAYWRIGHT_POLISH_QUEUED_URL);
  expect(await visibleText(queued)).toContain("Tip:");
  await runCommand(queued, "queue the first submission");
  await expect.poll(() => visibleText(queued)).not.toContain("Tip:");
  await expect.poll(() => terminalText(queued)).toContain("Queued");
});

test("manual scroll survives streaming and reflow, then bottom re-arms", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_POLISH_URL);
  await page.keyboard.press("PageUp");
  const before = await visibleText(page);
  const anchor = before.match(/history row \d+/)?.[0];
  expect(anchor).toBeTruthy();
  await runCommand(page, "stream while anchored");
  await expect.poll(() => visibleText(page)).toContain(anchor ?? "history row");
  await page.setViewportSize({ width: 1100, height: 700 });
  await expect.poll(() => visibleText(page)).toContain(anchor ?? "history row");
  await page.keyboard.press("End");
  await expect.poll(() => visibleText(page), { timeout: 8_000 }).toContain("Offline fixture response.");

  const hydrated = await page.context().newPage();
  await openTerminal(hydrated, process.env.PLAYWRIGHT_POLISH_URL);
  for (let index = 0; index < 30; index += 1) await hydrated.keyboard.press("PageUp");
  const beforeHydration = await visibleText(hydrated);
  const hydrationAnchor = beforeHydration.match(/history row \d+/)?.[0];
  expect(hydrationAnchor).toBeTruthy();
  await hydrated.keyboard.press("PageUp");
  await expect.poll(() => visibleText(hydrated)).toContain(hydrationAnchor ?? "history row");

  const duplicate = await page.context().newPage();
  await duplicate.setViewportSize({ width: 560, height: 360 });
  await openTerminal(duplicate, process.env.PLAYWRIGHT_POLISH_ANCHOR_URL);
  await duplicate.keyboard.press("End");
  await duplicate.mouse.move(120, 80);
  for (let index = 0; index < 60 && await visibleTerminalRow(duplicate, "block-two-marker") < 0; index += 1) {
    await duplicate.mouse.wheel(0, -120);
  }
  const markerRow = await stableVisibleTerminalRow(duplicate, "block-two-marker");
  expect(markerRow).toBeGreaterThanOrEqual(0);
  await expect.poll(() => visibleText(duplicate)).toContain("duplicate browser");
  await duplicate.setViewportSize({ width: 980, height: 360 });
  await expect.poll(() => visibleTerminalRow(duplicate, "block-two-marker")).toBe(markerRow);
  const afterReflow = await visibleText(duplicate);
  expect(afterReflow).toContain("duplicate browser");
  expect(afterReflow).not.toContain("block-one-marker");
});

test("display settings persist and ASCII suppresses only scrollbar rendering", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_POLISH_URL);
  await runCommand(page, "/timestamps");
  await expect.poll(() => terminalText(page)).toContain("Message timestamps shown.");
  await runCommand(page, "/scrollbar");
  await expect.poll(() => terminalText(page)).toContain("Chat scrollbar shown.");
  await runCommand(page, "/line-numbers");
  await expect.poll(() => terminalText(page)).toContain("Diff line numbers hidden for new diffs.");
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("fixture:polish-model");
  await runCommand(page, "/timestamps");
  await expect.poll(() => terminalText(page)).toContain("Message timestamps hidden.");
  await runCommand(page, "/scrollbar");
  await expect.poll(() => terminalText(page)).toContain("Chat scrollbar hidden.");
  await runCommand(page, "/line-numbers");
  await expect.poll(() => terminalText(page)).toContain("Diff line numbers shown for new diffs.");
});

test("theme preview, cancel, ANSI reset, persistence, and narrow modal preserve the terminal", async ({ context, page }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await openTerminal(page, process.env.PLAYWRIGHT_POLISH_URL);
  await expect.poll(() => terminalBackground(page)).toBe("#11121D");
  await runCommand(page, "/theme");
  await expect.poll(() => visibleText(page)).toContain("LangChain Dark (current)");
  await page.keyboard.press("ArrowDown");
  await expect.poll(() => terminalBackground(page)).toBe("#F5F5F7");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalBackground(page)).toBe("#11121D");
  await runCommand(page, "/theme");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalBackground(page)).toBe("#F5F5F7");
  await page.keyboard.type("theme clipboard intent", { delay: 12 });
  await page.keyboard.press("Control+c");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("theme clipboard intent");
  await expect.poll(() => terminalBackground(page)).toBe("#F5F5F7");
  await page.keyboard.press("Escape");
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalBackground(page)).toBe("#F5F5F7");

  await runCommand(page, "/theme");
  for (let index = 0; index < 32; index += 1) {
    await page.keyboard.press("ArrowDown");
    if ((await visibleText(page)).includes("Terminal ANSI") &&
        (await terminalBackground(page)) === "#11121D") break;
  }
  expect(await visibleText(page)).toContain("Terminal ANSI");
  await expect.poll(() => terminalBackground(page)).toBe("#11121D");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalBackground(page)).toBe("#F5F5F7");

  const narrow = await context.newPage();
  await narrow.setViewportSize({ width: 360, height: 260 });
  await openTerminal(narrow, process.env.PLAYWRIGHT_POLISH_ASCII_URL);
  await runCommand(narrow, "/theme ");
  await expect.poll(() => visibleText(narrow)).toContain("Select Theme");
  await narrow.evaluate(() => window.dacodeTerminal?.resize(20, 10));
  await expect.poll(() => narrow.evaluate(() => ({ cols: window.dacodeTerminal?.cols, rows: window.dacodeTerminal?.rows }))).toEqual({ cols: 20, rows: 10 });
  await expect.poll(() => visibleText(narrow)).toContain("LangChain");
  const bounds = await narrow.evaluate(() => {
    const terminal = window.dacodeTerminal;
    if (!terminal) return { overflow: true, composer: false };
    const buffer = terminal.buffer.active;
    const screen = document.querySelector<HTMLElement>(".xterm-screen");
    const host = document.querySelector<HTMLElement>(".xterm");
    const overflow = !screen || !host || screen.scrollWidth > screen.clientWidth ||
      screen.getBoundingClientRect().right > host.getBoundingClientRect().right + 1;
    let composer = false;
    let selection = false;
    for (let row = buffer.viewportY; row < buffer.viewportY + terminal.rows; row += 1) {
      const line = buffer.getLine(row)?.translateToString(true) ?? "";
			composer ||= line.includes("What would you");
      selection ||= line.includes("LangChain");
    }
    return { overflow, composer, selection };
  });
  expect(bounds).toEqual({ overflow: false, composer: true, selection: true });

  await page.keyboard.press("Control+d");
  await expect.poll(() => terminalBackground(page)).toBe("#11121D");
});
