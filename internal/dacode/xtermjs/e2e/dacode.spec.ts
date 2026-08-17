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

async function openTerminal(page: Page, url = "/"): Promise<void> {
  await page.goto(url);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
}

async function composerPaintedColumns(page: Page, text: string): Promise<number[] | undefined> {
  return page.evaluate((needle) => {
    const terminal = window.dacodeTerminal;
    if (!terminal) return undefined;
    const buffer = terminal.buffer.active;
    for (let row = 0; row < buffer.length; row += 1) {
      const line = buffer.getLine(row);
      if (!line?.translateToString(true).includes(needle)) continue;
      const paintedColumns: number[] = [];
      for (let column = 0; column < terminal.cols; column += 1) {
        if ((line.getCell(column)?.getBgColorMode() ?? 0) !== 0) paintedColumns.push(column);
      }
      return paintedColumns;
    }
    return undefined;
  }, text);
}

function shortThreadID(text: string): string | undefined {
  return text.match(/•\s+([0-9a-f]{7}…)/)?.[1];
}

test("renders the complete ready screen without browser overflow", async ({ page }) => {
  await openTerminal(page);

  const text = await terminalText(page);
  expect(text).toContain("dacode  Go coding agent");
  expect(text).toContain("What would you like to build?");
  expect(text).toContain("auto review");
  expect(text).toContain("openai:gpt-5.6-terra");
  expect(text).not.toContain("\u001b[");
  const borderedLines = text
    .split("\n")
    .filter((line) => line.startsWith("│"))
    .slice(0, 4);
  expect(borderedLines).toHaveLength(4);
  const rightBorders = borderedLines.map((line) => line.lastIndexOf("│"));
  expect(Math.min(...rightBorders)).toBeGreaterThan(0);
  expect(new Set(rightBorders).size).toBe(1);

  const colors = await page.evaluate(() => {
    const terminal = window.dacodeTerminal;
    const foregrounds = new Set<number>();
    const backgrounds = new Set<number>();
    if (!terminal) return { foregrounds: 0, backgrounds: 0 };
    const buffer = terminal.buffer.active;
    for (let row = 0; row < buffer.length; row += 1) {
      const line = buffer.getLine(row);
      if (!line) continue;
      for (let column = 0; column < terminal.cols; column += 1) {
        const cell = line.getCell(column);
        if (!cell) continue;
        if (cell.getFgColor() >= 0) foregrounds.add(cell.getFgColor());
        if (cell.getBgColor() >= 0) backgrounds.add(cell.getBgColor());
      }
    }
    return { foregrounds: foregrounds.size, backgrounds: backgrounds.size };
  });
  expect(colors.foregrounds).toBeGreaterThanOrEqual(4);
  expect(colors.backgrounds).toBeGreaterThanOrEqual(2);

  const renderedColors = await page.evaluate(() => {
    const spans = Array.from(document.querySelectorAll<HTMLElement>(".xterm-rows span"));
    const colorOf = (text: string): string | undefined => {
      const element = spans.find((span) => span.textContent === text);
      return element ? getComputedStyle(element).color : undefined;
    };
    return {
      title: colorOf("dacode"),
      subtitle: colorOf("  Go coding agent")
    };
  });
  expect(renderedColors).toEqual({
    title: "rgb(121, 162, 247)",
    subtitle: "rgb(187, 154, 247)"
  });

  const layout = await page.evaluate(() => {
    const terminal = document.querySelector<HTMLElement>("#terminal");
    const screen = document.querySelector<HTMLElement>(".xterm-screen");
    return {
      bodyWidth: document.body.scrollWidth,
      bodyHeight: document.body.scrollHeight,
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      terminal: terminal?.getBoundingClientRect().toJSON(),
      screen: screen?.getBoundingClientRect().toJSON()
    };
  });
  expect(layout.bodyWidth).toBeLessThanOrEqual(layout.viewportWidth);
  expect(layout.bodyHeight).toBeLessThanOrEqual(layout.viewportHeight);
  expect(layout.terminal?.width).toBeGreaterThan(0);
  expect(layout.screen?.height).toBeLessThanOrEqual(layout.terminal?.height ?? 0);

  const terminalRows = await page.evaluate(() => {
    const terminal = window.dacodeTerminal;
    if (!terminal) return undefined;
    const buffer = terminal.buffer.active;
    let lastContentRow = -1;
    for (let row = 0; row < buffer.length; row += 1) {
      if ((buffer.getLine(row)?.translateToString(true).trim() ?? "") !== "") lastContentRow = row;
    }
    return { lastContentRow, rows: terminal.rows };
  });
  expect(terminalRows?.lastContentRow).toBe((terminalRows?.rows ?? 0) - 1);
});

test("does not paint a background across the composer row", async ({ page }) => {
  await openTerminal(page);

  const composer = await page.evaluate(() => {
    const terminal = window.dacodeTerminal;
    if (!terminal) return undefined;
    const buffer = terminal.buffer.active;
    let bufferRow = -1;
    for (let row = 0; row < buffer.length; row += 1) {
      if (buffer.getLine(row)?.translateToString(true).includes("> What would you like to build?")) {
        bufferRow = row;
        break;
      }
    }
    if (bufferRow < 0) return undefined;
    const line = buffer.getLine(bufferRow);
    const paintedColumns: number[] = [];
    for (let column = 0; column < terminal.cols; column += 1) {
      if ((line?.getCell(column)?.getBgColorMode() ?? 0) !== 0) paintedColumns.push(column);
    }
    const renderedRow = Array.from(document.querySelectorAll<HTMLElement>(".xterm-rows > div")).find(
      (element) => element.textContent?.includes("> What would you like to build?")
    );
    return {
      paintedColumns,
      rowBackground: renderedRow ? getComputedStyle(renderedRow).backgroundColor : undefined,
      spanBackgrounds: renderedRow
        ? Array.from(
            new Set(
              Array.from(renderedRow.querySelectorAll("span"), (span) => getComputedStyle(span).backgroundColor)
            )
          )
        : []
    };
  });

  expect(composer).toBeDefined();
  expect(composer?.paintedColumns).toEqual([]);
  expect(composer?.rowBackground).toBe("rgba(0, 0, 0, 0)");
  expect(composer?.spanBackgrounds).not.toContain("rgb(17, 18, 29)");
  expect(composer?.spanBackgrounds).not.toContain("rgb(26, 27, 46)");

  await page.keyboard.type("visible draft");
  await expect.poll(() => terminalText(page)).toContain("> visible draft");
  expect(await composerPaintedColumns(page, "> visible draft")).toEqual([]);
});

test("clears a draft with ctrl+c without closing the session", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("discard this draft");
  await expect.poll(() => terminalText(page)).toContain("> discard this draft");
  await page.keyboard.press("Control+c");
  await expect.poll(() => terminalText(page)).not.toContain("discard this draft");
  await expect.poll(() => terminalText(page)).toContain("> What would you like to build?");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
});

test("keeps a non-empty draft open on ctrl+d", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("unfinished draft");
  await page.keyboard.press("Control+d");
  await expect.poll(() => terminalText(page)).toContain("> unfinished draft");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
});

test("edits the draft with navigation and deletion keys", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("helo");
  await page.keyboard.press("ArrowLeft");
  await page.keyboard.type("l");
  await expect.poll(() => terminalText(page)).toContain("> hello");
  await page.keyboard.press("Backspace");
  await expect.poll(() => terminalText(page)).toContain("> helo");
});

test("accepts ctrl+j as a composer newline without submitting", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("first line");
  await page.keyboard.press("Control+j");
  await page.keyboard.type("second line");
  await expect.poll(() => terminalText(page)).toContain("first line");
  await expect.poll(() => terminalText(page)).toContain("second line");
  const text = await terminalText(page);
  expect(text).not.toContain("Thinking");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
});

test("animates the spinner while a response is pending", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("wait for the response");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Thinking…");
  await expect.poll(() => terminalText(page)).toContain("> Agent is working…");

  const frames = await page.evaluate(async () => {
    const visibleFrame = (): string => {
      const terminal = window.dacodeTerminal;
      if (!terminal) return "";
      const buffer = terminal.buffer.active;
      for (let row = 0; row < buffer.length; row += 1) {
        const text = buffer.getLine(row)?.translateToString(true) ?? "";
        if (text.includes("Thinking…")) return text.trimStart().charAt(0);
      }
      return "";
    };
    const observed: string[] = [];
    for (let sample = 0; sample < 8; sample += 1) {
      observed.push(visibleFrame());
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    return observed.filter(Boolean);
  });

  expect(new Set(frames).size).toBeGreaterThan(1);
});

test("marks fast parallel filesystem tools complete before a slow sibling", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_YOLO_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show each completed parallel tool immediately");
  await page.keyboard.press("Enter");

  await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("✓ ls");
  await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("✓ read_file");
  const whileExecuteRuns = await terminalText(page);
  expect(whileExecuteRuns).toContain("○ execute");
  expect(whileExecuteRuns).not.toContain("Parallel tool batch finished.");
  expect(whileExecuteRuns).toContain("> Agent is working…");

  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("✓ execute");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Parallel tool batch finished.");
});

test("refits the TUI when the browser becomes narrow", async ({ page }) => {
  await openTerminal(page);
  const desktopColumns = await page.evaluate(() => window.dacodeTerminal?.cols ?? 0);

  await page.setViewportSize({ width: 480, height: 640 });
  await expect
    .poll(() => page.evaluate(() => window.dacodeTerminal?.cols ?? 0))
    .toBeLessThan(desktopColumns);
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  await page.waitForTimeout(100);
  const text = await terminalText(page);
  expect(text).toContain("openai:gpt-5.6-terra  •  auto review");
  expect(text).toContain("ctrl+d quit");
  expect(text).toContain("0 tok");
  expect(text).not.toContain("0 tok 0 tokens");
  expect(text).not.toContain(" • thread");

  const overflow = await page.evaluate(() => ({
    horizontal: document.body.scrollWidth - window.innerWidth,
    vertical: document.body.scrollHeight - window.innerHeight
  }));
  expect(overflow.horizontal).toBeLessThanOrEqual(0);
  expect(overflow.vertical).toBeLessThanOrEqual(0);
});

test("scrolls transcript history with the mouse wheel", async ({ page }) => {
  await openTerminal(page);

  for (let index = 0; index < 30; index += 1) {
    await page.keyboard.type(`/scroll-history-${index}`);
    await page.keyboard.press("Enter");
  }
  await expect.poll(() => terminalText(page)).toContain("Unknown command: /scroll-history-29");

  const screen = page.locator(".xterm-screen");
  await screen.hover();
  await page.mouse.wheel(0, -1_200);

  await expect.poll(() => terminalText(page)).not.toContain("Unknown command: /scroll-history-29");
  await expect
    .poll(() => terminalText(page))
    .toMatch(/Unknown command: \/scroll-history-(?:[0-9]|1[0-9])/);

  // Cursor blink messages refresh the TUI roughly twice a second. Manual
  // scrolling must survive those otherwise-unrelated redraws.
  await page.waitForTimeout(1_200);
  await expect.poll(() => terminalText(page)).not.toContain("Unknown command: /scroll-history-29");
  await expect
    .poll(() => terminalText(page))
    .toMatch(/Unknown command: \/scroll-history-(?:[0-9]|1[0-9])/);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
});

test("scrolls immediately after an agent response without keyboard input", async ({ page }) => {
  await openTerminal(page);

  for (let index = 0; index < 30; index += 1) {
    await page.keyboard.type(`/post-response-history-${index}`);
    await page.keyboard.press("Enter");
  }
  await expect.poll(() => terminalText(page)).toContain("Unknown command: /post-response-history-29");

  await page.keyboard.type("finish this response, then leave the transcript scrollable");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Thinking…");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("> What would you like to build?");

  // Simulate the intermittent xterm mode loss seen after a final response
  // frame. The browser's explicit wheel forwarding must not depend on it.
  await page.evaluate(() => new Promise<void>((resolve) => {
    window.dacodeTerminal?.write("\u001b[?1002l\u001b[?1006l", resolve);
  }));
  const mouseTrackingMode = await page.evaluate(() => (window.dacodeTerminal as any)?.modes?.mouseTrackingMode);
  expect(mouseTrackingMode).toBe("none");

  // Browser focus can leave xterm after the last response frame. A real mouse
  // wheel should restore terminal focus without requiring a sacrificial key.
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());

  const beforeScroll = await terminalText(page);
  const beforeFirstLine = Number(beforeScroll.match(/response line (\d+)/)?.[1] ?? -1);
  expect(beforeFirstLine).toBeGreaterThanOrEqual(0);
  const screen = page.locator(".xterm-screen");
  await screen.hover();
  await page.mouse.wheel(0, -1_200);
  let afterUpFirstLine = beforeFirstLine;
  await expect.poll(async () => {
    const text = await terminalText(page);
    afterUpFirstLine = Number(text.match(/response line (\d+)/)?.[1] ?? -1);
    return afterUpFirstLine;
  }).toBeLessThan(beforeFirstLine);
  await page.mouse.wheel(0, 1_200);
  await expect.poll(async () => {
    const text = await terminalText(page);
    return Number(text.match(/response line (\d+)/)?.[1] ?? -1);
  }).toBeGreaterThan(afterUpFirstLine);
});

test("shows manual review mode when requested", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_MANUAL_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);
  const text = await terminalText(page);
  expect(text).toContain("manual review");
  expect(text).not.toContain("auto review");
});

test("shows unrestricted mode when requested", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_YOLO_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);
  const text = await terminalText(page);
  expect(text).toContain("yolo");
  expect(text).not.toContain("auto review");
});

test("opens the startup session picker and resumes the selected session", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_RESUME_URL;
  expect(url).toBeTruthy();
  await page.goto(url!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Previous sessions");

  const picker = await terminalText(page);
  expect(picker).toContain("Newer browser task");
  expect(picker).toContain("Older browser task");
  expect(picker).toContain("DIRECTORY");
  expect(picker).toContain("~/browser-fixture");
  expect(picker.indexOf("Newer browser task")).toBeLessThan(picker.indexOf("Older browser task"));

  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Older browser answer");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  expect(await terminalText(page)).not.toContain("Previous sessions");
});

test("quits the startup resume screen with q", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_RESUME_URL;
  expect(url).toBeTruthy();
  await page.goto(url!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Previous sessions");

  await page.keyboard.press("q");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "closed");
  await expect(page.getByRole("status")).toHaveText("Session ended");
});

test("resumes a known session directly from the subcommand", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_DIRECT_RESUME_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);
  const text = await terminalText(page);
  expect(text).toContain("Newer browser task");
  expect(text).toContain("Newer browser answer");
  expect(text).not.toContain("Previous sessions");
});

test("accepts keyboard input and renders local slash-command output", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("/help");
  await page.keyboard.press("Enter");
  await expect
    .poll(() => terminalText(page))
    .toContain("Commands: /help  /clear  /new  /threads  /model  /goal  /workflow  /workflows  /quit");

  await page.keyboard.type("/model");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Model: openai:gpt-5.6-terra");
});

test("opens the workflow control panel and completes a saved workflow", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("/workflows");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("WORKFLOW CONTROL");
  await expect.poll(() => terminalText(page)).toContain("No workflow runs yet.");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");

  await page.keyboard.type("/workflow internal/dacode/xtermjs/testdata/workflows/complete.js");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("browser-release-sweep");
  await expect.poll(() => terminalText(page)).toContain("SUCCESS");
  const text = await terminalText(page);
  expect(text).toContain("SELECTED RUN");
  expect(text).toContain("● Inspect");
  expect(text).toContain("Fixture complete");
  expect(text).toContain("esc return");
});

test("cancels a running workflow from the workflow control panel", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("/workflow internal/dacode/xtermjs/testdata/workflows/cancellable.js");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("browser-cancellable-sweep");
  await expect.poll(() => terminalText(page)).toContain("RUNNING");
  await expect.poll(() => terminalText(page)).toContain("browser-check");
  await expect.poll(() => terminalText(page)).toContain("RUNNING AGENTS  1");
  const initialLine = (await terminalText(page)).match(/browser-check\s+Inspect\s+(\d+)s\s+·\s+~([\d.]+)(k?) tok/);
  const initialElapsed = Number(initialLine?.[1] ?? -1);
  const initialTokens = Number(initialLine?.[2] ?? 0) * (initialLine?.[3] === "k" ? 1_000 : 1);
  expect(initialElapsed).toBeGreaterThanOrEqual(0);
  expect(initialTokens).toBeGreaterThan(0);
  await page.waitForTimeout(1_100);
  const laterElapsed = Number((await terminalText(page)).match(/browser-check\s+Inspect\s+(\d+)s\s+·\s+~[\d.]+k? tok/)?.[1] ?? -1);
  expect(laterElapsed).toBeGreaterThan(initialElapsed);

  await page.keyboard.press("c");
  await expect.poll(() => terminalText(page)).toContain("CANCELLED");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
});

test("updates running workflow token estimates from streamed worker output", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("/workflow internal/dacode/xtermjs/testdata/workflows/token-progress.js");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("browser-token-progress");
  await expect.poll(() => terminalText(page)).toContain("RUNNING AGENTS  1");

  const displayedTokens = async (): Promise<number> => {
    const match = (await terminalText(page)).match(/token-progress-worker\s+Inspect\s+\d+s\s+·\s+~([\d.]+)(k?) tok/);
    return Number(match?.[1] ?? 0) * (match?.[2] === "k" ? 1_000 : 1);
  };
  const initial = await expect.poll(displayedTokens).toBeGreaterThan(0).then(() => displayedTokens());
  await expect.poll(displayedTokens).toBeGreaterThan(initial);
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("SUCCESS");
});

test("maps refactoring opportunities with a realistic deterministic workflow", async ({ page, request }) => {
  test.setTimeout(2 * 60_000);
  const url = process.env.PLAYWRIGHT_WORKFLOW_FAKE_URL;
  const apiURL = process.env.PLAYWRIGHT_WORKFLOW_FAKE_API_URL;
  expect(url).toBeTruthy();
  expect(apiURL).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("Map out refactoring opportunities in this repo with a workflow. Do not modify files.");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 30_000 }).toMatch(/\bwf_\d+\b/);
  await expect
    .poll(() => terminalText(page), { timeout: 90_000 })
    .toMatch(/Workflow repository-refactoring-map \(wf_\d+\) completed: (?:SUCCESS|ERROR)/);
  const completionText = await terminalText(page);
  if (!/Workflow repository-refactoring-map \(wf_\d+\) completed: SUCCESS/.test(completionText)) {
    const diagnosticResponse = await request.get(`${apiURL}/fixture-state`);
    throw new Error(`workflow failed; fixture state: ${JSON.stringify(await diagnosticResponse.json())}`);
  }
  await expect.poll(() => terminalText(page), { timeout: 30_000 }).toContain(
    "Partial workflow result reviewed: three refactoring findings were independently verified; two scanners failed"
  );

  const fixtureResponse = await request.get(`${apiURL}/fixture-state`);
  expect(fixtureResponse.ok()).toBe(true);
  const fixture = await fixtureResponse.json();
  expect(fixture).toMatchObject({
    chosenFailureCalls: 1,
    deniedApprovalReviews: 2,
    scoutCalls: 1,
    foregroundWorkflowCalls: 1,
    recoveryAlternativeCalls: 1,
    recoveryContinuations: 2,
    workerExecuteCalls: 8,
    workerExecuteContinuations: 6
  });
  expect(fixture.approvalReviews).toBeGreaterThanOrEqual(8);
  expect(fixture.failedWorkerRequests).toBeGreaterThanOrEqual(1);

  await page.keyboard.type("/workflows");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("WORKFLOW CONTROL");
  const text = await terminalText(page);
  expect(text).toContain("SUCCESS");
  expect(text).toContain("7 done · 0 active · 2 failed");
  expect(text).toContain("Grounded parallel repository review");
  expect(text).not.toContain("no agent calls");
});

test("automatic review never flashes manual controls or prints approvals", async ({ page }) => {
  test.setTimeout(60_000);
  const url = process.env.PLAYWRIGHT_WORKFLOW_FAKE_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  const frames: string[] = [];
  await page.keyboard.type("AUTO_REVIEW_VISIBILITY: exercise one approval-gated action.");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 20_000 }).toContain("Reviewing action");
  while (!(await terminalText(page)).includes("Automatic review completed without displaying manual controls.")) {
    frames.push(await terminalText(page));
    await page.waitForTimeout(10);
  }
  frames.push(await terminalText(page));

  expect(frames.some((frame) => frame.includes("Reviewing action"))).toBe(true);
  for (const frame of frames) {
    expect(frame).not.toContain("Review requested");
    expect(frame).not.toContain("y approve");
    expect(frame).not.toContain("n reject");
    expect(frame).not.toContain("Automatic review approved");
  }
});

test("live OpenAI maps repository refactoring opportunities with a workflow", async ({ page }) => {
  test.skip(process.env.DAGO_PLAYWRIGHT_OPENAI_LIVE !== "1", "set DAGO_PLAYWRIGHT_OPENAI_LIVE=1 to run");
  test.setTimeout(30 * 60_000);
  const url = process.env.PLAYWRIGHT_OPENAI_LIVE_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type(
    "Map out refactoring opportunities in this repo with a workflow. Do not modify files. " +
      "Keep this live test bounded to 4 independent scan agents plus at most 2 synthesis or verification agents."
  );
  await page.keyboard.press("Enter");
  await expect
    .poll(() => terminalText(page), { timeout: 6 * 60_000 })
    .toMatch(/\bwf_\d+\b/);
  await expect
    .poll(() => terminalText(page), { timeout: 22 * 60_000 })
    .toMatch(/Workflow .+ \(wf_\d+\) completed: SUCCESS/);
  await expect.poll(() => terminalText(page), { timeout: 2 * 60_000 }).toContain(
    "> What would you like to build?"
  );
  await expect
    .poll(() => terminalText(page), { timeout: 2 * 60_000 })
    .not.toMatch(/Responding…|Reviewing workflow result/);

  await page.keyboard.type("/workflows");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("WORKFLOW CONTROL");
  const text = await terminalText(page);
  expect(text).toContain("SUCCESS");
  expect(text).toMatch(/[1-9]\d* done · 0 active · 0 failed/);
  expect(text).not.toContain("no agent calls");
});

test("sets and pauses a goal", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("/goal Finish the release checklist");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Goal set. Finish the release checklist");
  await expect.poll(() => terminalText(page)).toContain("goal:active");
  await expect.poll(() => terminalText(page)).toContain("Continuing goal…");

  await page.keyboard.press("Control+c");
  await expect.poll(() => terminalText(page)).toContain("Operation cancelled.");

  await page.keyboard.type("/goal pause");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Goal paused.");
  await expect.poll(() => terminalText(page)).toContain("goal:paused");
});

test("reports unknown slash commands and clears local output", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("/does-not-exist");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Unknown command: /does-not-exist");

  await page.keyboard.type("/clear");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).not.toContain("Unknown command: /does-not-exist");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
});

test("starts a new local thread and clears transcript output", async ({ page }) => {
  await openTerminal(page);
  const originalThread = shortThreadID(await terminalText(page));
  expect(originalThread).toBeDefined();

  await page.keyboard.type("/help");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Commands: /help");
  await page.keyboard.type("/new");
  await page.keyboard.press("Enter");

  await expect.poll(async () => shortThreadID(await terminalText(page))).not.toBe(originalThread);
  await expect.poll(() => terminalText(page)).not.toContain("Commands: /help  /clear");
});

test("closes the browser session with the local quit command", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("/quit");
  await page.keyboard.press("Enter");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "closed");
  await expect(page.getByRole("status")).toHaveText("Session ended");
});

test("closes the browser session cleanly on ctrl+d", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.press("Control+d");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "closed");
  await expect(page.getByRole("status")).toHaveText("Session ended");
});
