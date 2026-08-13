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
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
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

test("accepts keyboard input and renders local slash-command output", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("/help");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Commands: /help  /clear  /new  /model  /quit");

  await page.keyboard.type("/model");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Model: openai:gpt-5.6-terra");
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
