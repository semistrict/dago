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

async function visibleTerminalLines(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const terminal = window.dacodeTerminal;
    if (!terminal) return [];
    const buffer = terminal.buffer.active;
    const lines: string[] = [];
    for (let row = buffer.viewportY; row < buffer.viewportY + terminal.rows; row += 1) {
      lines.push(buffer.getLine(row)?.translateToString(true) ?? "");
    }
    return lines;
  });
}

async function openTerminal(page: Page, url = "/"): Promise<void> {
  await page.goto(url);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
}

async function pageUpUntil(page: Page, needle: string, attempts = 20): Promise<string> {
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    const text = await terminalText(page);
    if (text.includes(needle)) return text;
    await page.keyboard.press("PageUp");
    await page.waitForTimeout(25);
  }
  return terminalText(page);
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

async function clickTerminalText(page: Page, text: string, columnOffset = 1): Promise<void> {
  const point = await page.evaluate(
    ({ needle, offset }) => {
      const terminal = window.dacodeTerminal;
      const screen = document.querySelector<HTMLElement>(".xterm-screen");
      if (!terminal || !screen) return undefined;
      const buffer = terminal.buffer.active;
      for (let row = buffer.viewportY; row < buffer.viewportY + terminal.rows; row += 1) {
        const line = buffer.getLine(row)?.translateToString(true) ?? "";
        const column = line.indexOf(needle);
        if (column < 0) continue;
        const bounds = screen.getBoundingClientRect();
        return {
          x: bounds.left + ((column + offset + 0.5) * bounds.width) / terminal.cols,
          y: bounds.top + ((row - buffer.viewportY + 0.5) * bounds.height) / terminal.rows
        };
      }
      return undefined;
    },
    { needle: text, offset: columnOffset }
  );
  expect(point).toBeDefined();
  await page.mouse.click(point?.x ?? 0, point?.y ?? 0);
}

function shortThreadID(text: string): string | undefined {
	const full = text.match(/Thread ID:\s+([0-9a-f]{7,})/)?.[1];
	return full?.slice(0, 7);
}

function fullThreadID(text: string): string | undefined {
	return text.match(/Thread ID:\s+([0-9a-f]{7,})/)?.[1];
}

test("renders the complete ready screen without browser overflow", async ({ page }) => {
  await openTerminal(page);

  const text = await terminalText(page);
  expect(text).toContain("│  dacode");
  expect(text).toContain("Ready to code. What would you like to build?");
  expect(text).toContain("What would you like to build?");
	expect(text).toContain("•  Auto");
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
  await expect(page.locator("html")).toHaveAttribute("data-terminal-background", "#11121D");

  const renderedColors = await page.evaluate(() => {
    const spans = Array.from(document.querySelectorAll<HTMLElement>(".xterm-rows span"));
    const colorOf = (text: string): string | undefined => {
      const element = spans.find((span) => span.textContent?.startsWith(text));
      return element ? getComputedStyle(element).color : undefined;
    };
    return { title: colorOf("dacode") };
  });
  expect(renderedColors).toEqual({ title: "rgb(121, 162, 247)" });

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
  expect(composer?.paintedColumns.length).toBeLessThanOrEqual(1);
  expect(composer?.rowBackground).toBe("rgba(0, 0, 0, 0)");

  await page.keyboard.type("visible draft");
  await expect.poll(() => terminalText(page)).toContain("> visible draft");
  expect((await composerPaintedColumns(page, "> visible draft"))?.length).toBeLessThanOrEqual(1);
});

test("previews, cancels, and persists built-in, custom, and terminal themes", async ({ page }) => {
  const themeURL = process.env.PLAYWRIGHT_THEME_URL;
  expect(themeURL).toBeTruthy();
  await openTerminal(page, themeURL);

  const titleColor = async (): Promise<string | undefined> =>
    page.evaluate(() => {
      const title = Array.from(document.querySelectorAll<HTMLElement>(".xterm-rows span")).find(
        (element) => element.textContent === "Select Theme"
      );
      return title ? getComputedStyle(title).color : undefined;
    });

  await page.keyboard.type("/theme");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Select Theme");
  await expect.poll(() => terminalText(page)).toContain("Browser Custom");
  await expect.poll(() => terminalText(page)).toContain("Terminal ANSI Dark");
  const original = await titleColor();
  await page.keyboard.press("ArrowDown");
  await expect.poll(titleColor).not.toBe(original);
  await page.keyboard.press("n");
  await expect.poll(() => terminalText(page)).toContain("langchain-light");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).not.toContain("Select Theme");

  await page.keyboard.type("/theme");
  await page.keyboard.press("Enter");
  await page.keyboard.press("ArrowDown");
  const persisted = await titleColor();
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).not.toContain("Select Theme");
  await expect.poll(() => terminalText(page)).toContain("Theme preference saved.");
  await page.reload();
  await openTerminal(page, themeURL);
  await page.keyboard.type("/theme");
  await page.keyboard.press("Enter");
  await expect.poll(titleColor).toBe(persisted);

  await page.keyboard.press("ArrowDown");
  const terminalDefault = await titleColor();
  await page.keyboard.press("t");
  await expect.poll(() => terminalText(page)).toContain("current, default");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Terminal theme default saved.");
  await page.reload();
  await openTerminal(page, themeURL);
  await page.keyboard.type("/theme");
  await page.keyboard.press("Enter");
  await expect.poll(titleColor).toBe(terminalDefault);
  await page.keyboard.press("Escape");
});

test("copies a draft with ctrl+c without clearing or closing the session", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await openTerminal(page);

  await page.keyboard.type("copy this draft");
  await expect.poll(() => terminalText(page)).toContain("> copy this draft");
  await page.keyboard.press("Control+c");
  await expect(page.locator("html")).toHaveAttribute("data-clipboard-state", "copied");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("copy this draft");
  await expect.poll(() => terminalText(page)).toContain("> copy this draft");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
});

test("Ctrl+C does not shift a full transcript upward or leave its final row blank", async ({ page }) => {
  await openTerminal(page);

	for (let index = 0; index < 20; index += 1) {
		await page.keyboard.type(`/ctrl-c-layout-${index}`);
		await page.keyboard.press("Enter");
	}
	const marker = "Unknown command: /ctrl-c-layout-19";
	await expect.poll(() => terminalText(page)).toContain(marker);
	const before = await visibleTerminalLines(page);
	const markerRow = before.findIndex((line) => line.includes(marker));
	expect(markerRow).toBeGreaterThanOrEqual(0);
	expect(before.at(-1)?.trim()).not.toBe("");

  await page.keyboard.press("Control+c");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
	await expect.poll(async () => {
		const lines = await visibleTerminalLines(page);
		return {
			markerRow: lines.findIndex((line) => line.includes(marker)),
			lastRowBlank: (lines.at(-1)?.trim() ?? "") === ""
		};
	}).toEqual({ markerRow, lastRowBlank: false });
});

test("a wrapped two-line draft keeps its first line visible while editing the second", async ({ page }) => {
  await page.setViewportSize({ width: 640, height: 420 });
  await openTerminal(page);

  const firstLine = `FIRST-DRAFT-LINE-${"a".repeat(80)}`;
  const secondLine = `SECOND-DRAFT-LINE-${"b".repeat(16)}`;
  await page.keyboard.type(firstLine, { delay: 5 });
  await page.keyboard.press("Control+j");
  await page.keyboard.type(secondLine, { delay: 5 });

  await expect.poll(() => terminalText(page)).toContain(secondLine);
  await expect.poll(() => visibleTerminalLines(page).then((lines) => lines.join("\n"))).toContain(firstLine);
  await expect.poll(() => visibleTerminalLines(page).then((lines) => lines.join("\n"))).toContain(secondLine);
});

test("forward-deletes with ctrl+d and quits only at the end of the draft", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.insertText("abé界c");
  await page.keyboard.press("ArrowLeft");
  await page.keyboard.press("ArrowLeft");
  await page.keyboard.press("Control+d");
  await expect.poll(() => terminalText(page)).toContain("> abéc");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await page.keyboard.press("End");
  await page.keyboard.press("Control+d");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "closed");
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

test("automatically retries a transient model transport failure", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("retry a transient model transport failure");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Transient model transport recovered automatically.");
  await expect.poll(() => terminalText(page)).toContain("> What would you like to build?");

  const text = await terminalText(page);
  expect(text.match(/Transient model transport recovered automatically\./g)).toHaveLength(1);
  expect(text).not.toContain("response stream ended before completion");
  expect(text).not.toContain("read websocket response");
});

test("marks fast parallel filesystem tools complete before a slow sibling", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_TRANSCRIPT_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show each completed parallel tool immediately");
  await page.keyboard.press("Enter");

	await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("Ctrl+O toggle");
	await page.keyboard.press("Control+o");
  await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("✓ ls");
  await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("✓ read_file");
  const whileExecuteRuns = await terminalText(page);
  expect(whileExecuteRuns).toContain("○ execute");
  expect(whileExecuteRuns).not.toContain("Parallel tool batch finished.");
  expect(whileExecuteRuns).toContain("> Agent is working…");
	expect(whileExecuteRuns).toContain("Using execute");
	expect(whileExecuteRuns).not.toContain("<U+001B CONTROL>");

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
  expect(text).toContain("openai:gpt-5.6-terra  •  Auto");
	expect(text).toContain("openai:gpt-5.6-terra");
	expect(text).toContain("Tokens: 0");
  expect(text).not.toContain("0 tok 0 tokens");
  expect(text).not.toContain(" • thread");

  const overflow = await page.evaluate(() => ({
    horizontal: document.body.scrollWidth - window.innerWidth,
    vertical: document.body.scrollHeight - window.innerHeight
  }));
  expect(overflow.horizontal).toBeLessThanOrEqual(0);
  expect(overflow.vertical).toBeLessThanOrEqual(0);
});

test("mouse wheel scrolls an overflowing transcript without changing recalled input history", async ({ page }) => {
  await openTerminal(page);

  for (let index = 0; index < 6; index += 1) {
    await page.keyboard.type(`/wheel-history-${index}`);
    await page.keyboard.press("Enter");
  }
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Unknown command: /wheel-history-5");

  const longPrompt = "finish this response, then leave the transcript scrollable";
  await page.keyboard.type(longPrompt);
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("response line 49");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("> What would you like to build?");

  // Put a real persisted-history entry back in the edit buffer, then make the
  // draft unique. A wheel gesture misdecoded as ArrowUp would replace it with
  // the older history entry and fail this assertion.
  await page.keyboard.press("ArrowUp");
  await page.keyboard.type(" -- edited draft");
  const recalledDraft = `${longPrompt} -- edited draft`;
  await expect.poll(() => terminalText(page)).toContain(`> ${recalledDraft}`);

  // Remove xterm's application mouse mode to reproduce the real failure mode:
  // without the dedicated wheel transport xterm translates wheel gestures to
  // cursor keys, which navigate the input-history entry currently being edited.
  await page.evaluate(() => new Promise<void>((resolve) => {
    window.dacodeTerminal?.write("\u001b[?1002l\u001b[?1006l", resolve);
  }));
  expect(await page.evaluate(() => (window.dacodeTerminal as any)?.modes?.mouseTrackingMode)).toBe("none");

  const screen = page.locator(".xterm-screen");
  const bounds = await screen.boundingBox();
  const rows = await page.evaluate(() => window.dacodeTerminal?.rows ?? 24);
  expect(bounds).not.toBeNull();

  // Wheel directly over the composer. It must still move the transcript and
  // must never be reinterpreted as ArrowUp/ArrowDown by the input widget.
  await page.mouse.move(
    (bounds?.x ?? 0) + (bounds?.width ?? 0) / 2,
    (bounds?.y ?? 0) + (bounds?.height ?? 0) * (rows - 3) / rows,
  );
  await page.mouse.wheel(0, -1_200);

  await expect.poll(() => terminalText(page)).not.toContain("response line 49");
  await expect.poll(() => terminalText(page)).toMatch(/response line (?:[0-9]|1[0-9])/);
  await expect.poll(() => terminalText(page)).toContain(`> ${recalledDraft}`);
  expect(await page.evaluate(() => window.dacodeTerminal?.buffer.active.baseY ?? -1)).toBe(0);

  // Exercise both directions repeatedly over transcript and composer areas.
  // The draft must remain byte-for-byte stable throughout.
  await page.mouse.move((bounds?.x ?? 0) + 20, (bounds?.y ?? 0) + (bounds?.height ?? 0) / 3);
  await page.mouse.wheel(0, -600);
  await expect.poll(() => terminalText(page)).toContain(`> ${recalledDraft}`);
  await page.mouse.wheel(0, 600);
  await expect.poll(() => terminalText(page)).toContain(`> ${recalledDraft}`);
  await page.mouse.move(
    (bounds?.x ?? 0) + (bounds?.width ?? 0) / 2,
    (bounds?.y ?? 0) + (bounds?.height ?? 0) * (rows - 3) / rows,
  );
  await page.mouse.wheel(0, 1_200);
  await expect.poll(() => terminalText(page)).toContain("response line 49");
  await expect.poll(() => terminalText(page)).toContain(`> ${recalledDraft}`);

  // Cursor blink messages refresh the TUI roughly twice a second. Manual
  // scrolling and the active draft must survive those unrelated redraws.
  await page.waitForTimeout(1_200);
  await expect.poll(() => terminalText(page)).toContain("response line 49");
  await expect.poll(() => terminalText(page)).toContain(`> ${recalledDraft}`);
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
  expect(text).toContain("manual");
	expect(text).not.toContain("•  Auto");
});

test("starts in Auto immediately and accepts input without a notice", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_AUTO_NOTICE_URL;
  expect(url).toBeTruthy();
  await page.goto(url!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
	await expect.poll(() => terminalText(page)).toContain("•  Auto");
  let text = await terminalText(page);
	expect(text).not.toContain("Enter to keep Auto");
	expect(text).not.toContain("Esc for Manual");

	await page.keyboard.type("input works immediately");
	await expect.poll(() => terminalText(page)).toContain("> input works immediately");

  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  text = await terminalText(page);
	expect(text).toContain("•  Auto");
  expect(text).not.toContain("Enter to keep Auto");
});

test("renders hidden approval characters with visible security warnings", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_MANUAL_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show security approval");
  await page.keyboard.press("Enter");

  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Requires Approval");
  const text = await terminalText(page);
  expect(text).toContain("<U+202E RIGHT-TO-LEFT OVERRIDE>");
  expect(text).toContain("Warning: execute.command: hidden Unicode");
  expect(text).not.toContain("\u202e");
});

test("renders write approval content and rejects with the numeric quick key", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_WIDGET_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show write approval");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain(">>> write_file Requires Approval <<<");
  const text = await terminalText(page);
  expect(text).toMatch(/Write file: .*\/preview\.md/);
  expect(text).toContain("first line");
  expect(text).toContain("second line");
  expect(text).toContain("1. Approve (y)");
  expect(text).toContain("2. Enable Auto for this thread (a)");
  expect(text).toContain("3. Reject (n)");
  expect(text).toContain("↑/↓ navigate");
  await page.keyboard.press("3");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Bare rejection received.");
});

test("renders an edit diff and rejects with n", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_WIDGET_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show edit approval");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain(">>> edit_file Requires Approval <<<");
  const text = await terminalText(page);
  expect(text).toMatch(/Edit file: .*\/preview\.md/);
  expect(text).toContain("--- before");
  expect(text).toContain("@@ -2,1 +2,1 @@");
  expect(text).toContain("-old value");
  expect(text).toContain("+new value");
  await page.keyboard.press("n");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Bare rejection received.");
	await expect.poll(() => terminalText(page)).toContain("! edit_file rejected");
});

test("renders delete approval and rejects with Escape", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_WIDGET_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show delete approval");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain(">>> delete Requires Approval <<<");
  const text = await terminalText(page);
  expect(text).toMatch(/Delete: .*\/preview\.md/);
  expect(text).toContain("+++ /dev/null");
  expect(text).toContain("-old value");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Bare rejection received.");
});

test("renders sorted generic arguments and supports menu navigation", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_WIDGET_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show generic approval");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain(">>> web_search Requires Approval <<<");
  const text = await terminalText(page);
  expect(text).toContain("max_results: 3");
  expect(text).toContain('query: "current release status"');
  expect(text.indexOf("max_results: 3")).toBeLessThan(text.indexOf('query: "current release status"'));
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("k");
  await page.keyboard.press("j");
  await page.keyboard.press("j");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Bare rejection received.");
});

test("hides sensitive file contents from approval scrollback", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_WIDGET_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show sensitive write approval");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Contents hidden - file may contain credentials");
  const text = await terminalText(page);
  expect(text).toMatch(/Write file: .*\/\.env\.local/);
  expect(text).not.toContain("SECRET=not-for-scrollback");
  await page.keyboard.press("n");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Bare rejection received.");
});

test("renders batched approvals and applies the selected decision to all", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_WIDGET_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show batch approval");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain(">>> 2 Tool Calls Require Approval <<<");
  const text = await terminalText(page);
  expect(text).toContain("1. write_file");
  expect(text).toContain("2. execute");
  expect(text).toContain("Approve all 2 (y)");
  expect(text).toContain("Reject all 2 (n)");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Bare rejection received.");
});

test("expands long shell commands without losing the pending decision", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_WIDGET_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show long approval");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("press e to");
  expect(await terminalText(page)).not.toContain("printf twelve");
  await page.keyboard.press("e");
  await expect.poll(() => terminalText(page)).toContain("printf twelve");
  expect(await terminalText(page)).not.toContain("press e to");
  await page.keyboard.press("n");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Bare rejection received.");
});

test("approves from the app-level y quick key", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_WIDGET_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show approval quick keys");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain(">>> execute Requires Approval <<<");
  await page.keyboard.press("y");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Approved action completed.");
});

test("approves from the numeric menu position", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_WIDGET_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show approval quick keys");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain(">>> execute Requires Approval <<<");
  await page.keyboard.press("1");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Approved action completed.");
});

test("enables Auto from the approval menu and approves the current call", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_WIDGET_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show approval quick keys");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain(">>> execute Requires Approval <<<");
  await page.keyboard.press("2");
	await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("•  Auto");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Approved action completed.");
	await expect.poll(() => terminalText(page)).toContain("•  Auto");
});

test("cancels and submits a bounded free-text rejection reason", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_MANUAL_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show reasoned approval");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Requires Approval");
  let text = await terminalText(page);
  expect(text).toContain("Tab reject with feedback");
  expect(text).toContain("Esc reject");

  await page.keyboard.press("Tab");
  await expect.poll(() => terminalText(page)).toContain("Reason (Enter to submit, Esc to cancel)");
  text = await terminalText(page);
  expect(text).toContain("leave blank to reject without a reason");
  expect(text).not.toContain("Tab reject with feedback");
  await page.keyboard.type("y avoid this call");
  await page.keyboard.press("Tab");
  await expect.poll(() => terminalText(page)).toContain("y avoid this call");
  await page.keyboard.press("n");
  await expect.poll(() => terminalText(page)).toContain("Tab reject with feedback");
  text = await terminalText(page);
  expect(text).toContain("Requires Approval");
  expect(text).not.toContain("Rejected: execute");

  await page.keyboard.press("Tab");
  await page.keyboard.type("use safer read check");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Rejection feedback received.");
  text = await terminalText(page);
  expect(text).toContain("Rejected: execute");
  expect(text).toContain("Reason: use safer read check");
});

test("submits a blank feedback field as a bare rejection", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_MANUAL_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show blank reason approval");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Requires Approval");
  await page.keyboard.press("Tab");
  await expect.poll(() => terminalText(page)).toContain("leave blank to reject without a reason");
  await page.keyboard.press("Enter");

  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Bare rejection received.");
  const text = await terminalText(page);
  expect(text).toContain("Rejected: execute");
  expect(text).not.toContain("Reason:");
});

test("defers approval shortcuts while the user is typing", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_MANUAL_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show deferred approval");
  await page.keyboard.press("Enter");
  await page.keyboard.type("draft y n stays input", { delay: 20 });
  await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("Waiting for typing to finish...");
  let text = await terminalText(page);
  expect(text).toContain("draft y n stays input");
  expect(text).not.toContain("Requires Approval");
  expect(text).not.toContain("Approved: execute");
  expect(text).not.toContain("Rejected: execute");

  await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("Requires Approval");
  text = await terminalText(page);
  expect(text).toContain("draft y n stays input");
  expect(text).not.toContain("Approved: execute");
  expect(text).not.toContain("Rejected: execute");
  await page.keyboard.press("n");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Bare rejection received.");
});

test("keeps decision shortcuts in the composer through Auto review fallback", async ({ page }) => {
  await openTerminal(page);

  // A first unavailable classifier batch is denied conservatively; retrying the
  // same unavailable classifier crosses the per-thread config-fault latch and
  // requires a human decision.
  await page.keyboard.type("show deferred auto approval");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Automatic review denied execute");

  await page.keyboard.type("show second deferred auto approval");
  await page.keyboard.press("Enter");
  await expect.poll(async () => ((await terminalText(page)).match(/Automatic review denied execute/g) ?? []).length, { timeout: 10_000 }).toBeGreaterThanOrEqual(2);

  await page.keyboard.type("show third deferred auto approval");
  await page.keyboard.press("Enter");
  await page.keyboard.type("auto y n draft", { delay: 20 });
  await expect.poll(() => terminalText(page), { timeout: 12_000 }).toContain("Waiting for typing to finish...");
  let text = await terminalText(page);
  expect(text).toContain("auto y n draft");
  expect(text).toContain("Automatic review unavailable; a user decision is required.");
  expect(text).not.toContain("Approved: execute");
  expect(text).not.toContain("Rejected: execute");

  await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("Requires Approval");
  text = await terminalText(page);
  expect(text).toContain("auto y n draft");
  expect(text).toContain("Switch to Manual (a)");
  expect(text).not.toContain("Approved: execute");
  expect(text).not.toContain("Rejected: execute");
  await page.keyboard.press("a");
  await expect.poll(() => terminalText(page)).toContain("manual");
  await expect.poll(() => terminalText(page)).toContain("Enable Auto for this thread (a)");
  await page.keyboard.press("n");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Bare rejection received.");
});

test("moves a deferred Manual approval into conservative Auto review", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_MANUAL_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show deferred auto approval");
  await page.keyboard.press("Enter");
  await page.keyboard.type("mode switch draft", { delay: 20 });
  await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("Waiting for typing to finish...");
  await page.keyboard.press("Control+t");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Automatic review denied execute");
  let text = await terminalText(page);
	expect(text).toContain("•  Auto");
  expect(text).toContain("mode switch draft");
  expect(text).not.toContain("Approved: execute");
  expect(text).not.toContain("Rejected: execute");
});

test("shows unrestricted mode when requested", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_YOLO_URL;
  expect(url).toBeTruthy();
  await page.goto(url!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("YOLO mode");
  let text = await terminalText(page);
  expect(text).toContain("without asking you first");
  expect(text).toContain("Enter to enable YOLO");
  expect(text).toContain("m for Manual");
  expect(text).toContain("Esc to keep current mode");

  await page.keyboard.press("m");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  await expect.poll(() => terminalText(page)).toContain("manual");
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("YOLO mode");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("manual");
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("YOLO mode");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  await expect.poll(() => terminalText(page)).toContain("•  YOLO");
  text = await terminalText(page);
  expect(text).toContain("•  YOLO");
	expect(text).not.toContain("•  Auto");
  expect(text).not.toContain("Enter to enable YOLO");

  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  text = await terminalText(page);
  expect(text).toContain("•  YOLO");
  expect(text).not.toContain("Enter to enable YOLO");
});

test("runs startup setup before invoking an initial project skill", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_STARTUP_URL;
  expect(url).toBeTruthy();
  await page.goto(url!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");

  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("startup command output");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("I'm invoking the skill `playwright-startup`.");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Playwright startup skill instructions.");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("**User request:** inspect startup automation");

  const text = await terminalText(page);
  expect(text).toContain("Running startup command:");
  expect(text.indexOf("startup command output")).toBeLessThan(text.indexOf("I'm invoking the skill"));
});

test("changes approval mode with slash commands and keyboard shortcuts", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("/manual");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toMatch(/\nmanual •/);

  await page.keyboard.type("/yolo");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("•  YOLO");

  await page.keyboard.press("Shift+Tab");
  await expect.poll(() => terminalText(page)).toMatch(/\nmanual •/);

  await page.keyboard.press("Control+t");
  await expect.poll(() => terminalText(page)).toMatch(/\nauto •/);
});

test("browses agents, toggles the default, cancels, and switches into a new thread", async ({ page }) => {
  await openTerminal(page);
  const originalThread = shortThreadID(await terminalText(page));
  expect(originalThread).toBeDefined();

  await page.keyboard.type("/agents");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Select Agent");
  await expect.poll(() => terminalText(page)).toContain("› dacode (current)");
  await expect.poll(() => terminalText(page)).toContain("research");

  await page.keyboard.press("Tab");
  await expect.poll(() => terminalText(page)).toContain("› research");
  await page.keyboard.press("Shift+Tab");
  await expect.poll(() => terminalText(page)).toContain("› dacode (current)");
  await page.keyboard.press("ArrowDown");
  await expect.poll(() => terminalText(page)).toContain("› research");

  await page.keyboard.press("Control+s");
  await expect.poll(() => terminalText(page)).toContain("research (default)");
  await expect.poll(() => terminalText(page)).toContain("Default set to research");
  await page.keyboard.press("Control+s");
  await expect.poll(() => terminalText(page)).toContain("Default cleared");
  expect(await terminalText(page)).not.toContain("research (default)");

  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  expect(await terminalText(page)).not.toContain("Select Agent");

  await page.keyboard.type("/agents");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Select Agent");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Switched to agent research. Started a new thread.");
  await expect.poll(() => terminalText(page)).toContain("agent:research");
  await expect.poll(async () => shortThreadID(await terminalText(page))).not.toBe(originalThread);
});

test("opens the startup session picker and resumes the selected session", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_RESUME_URL;
  expect(url).toBeTruthy();
  await page.goto(url!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Threads");

  const picker = await terminalText(page);
  expect(picker).toContain("Newer browser task");
  expect(picker).toContain("Older browser task");
  expect(picker).toContain("CWD");
  const newerRow = picker.split("\n").find((line) => line.includes("Newer browser task"));
  expect(newerRow).toBeDefined();
  expect(newerRow).toContain("2");
  expect(newerRow).toContain("dago");
  expect(picker.indexOf("Newer browser task")).toBeLessThan(picker.indexOf("Older browser task"));

  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Older browser answer");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  expect(await terminalText(page)).not.toContain("Threads");
});

test("quits the startup resume screen with q", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_RESUME_URL;
  expect(url).toBeTruthy();
  await page.goto(url!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Threads");

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

test("completes first-run onboarding, applies the model, and persists completion", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_ONBOARDING_URL;
  expect(url).toBeTruthy();
  await page.goto(url!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("What should the agent call you?");

  await page.keyboard.type("ada");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Optional integrations");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Select Model");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Enable web search?");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("How should Auto mode handle goal criteria?");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Welcome, Ada.");
  await expect.poll(() => terminalText(page)).toContain("Model set to");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");

  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  expect(await terminalText(page)).not.toContain("What should the agent call you?");
});

test("gates cross-agent resume through cwd and compact decisions", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_RESUME_FLOW_URL;
  expect(url).toBeTruthy();
  await page.goto(url!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Switch agents to resume?");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Resume from the thread's original directory?");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Compact this thread?");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 20_000 }).toContain("compact_conversation completed");
  await expect.poll(() => terminalText(page)).toContain("Conversation compacted.");
  await page.keyboard.type("/agents");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("› builder (current)");
  await page.keyboard.press("Escape");
});

test("forces direct offload and surfaces compaction failures", async ({ page }) => {
  const successURL = process.env.PLAYWRIGHT_OFFLOAD_URL;
  const errorURL = process.env.PLAYWRIGHT_OFFLOAD_ERROR_URL;
  expect(successURL).toBeTruthy();
  expect(errorURL).toBeTruthy();

  await page.goto(successURL!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("What would you like to build?");
  await page.keyboard.type("/offload");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 20_000 }).toContain("compact_conversation completed");
  await expect.poll(() => terminalText(page)).toContain("Conversation compacted.");

  await page.goto(errorURL!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("What would you like to build?");
  await page.keyboard.type("/offload");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 20_000 }).toContain("fixture summary unavailable");
  await expect.poll(() => terminalText(page)).toContain("Compaction failed:");
});

test("restarts an owned server and reports unavailable and failed restarts", async ({ page }) => {
  const successURL = process.env.PLAYWRIGHT_RESTART_SUCCESS_URL;
  const unavailableURL = process.env.PLAYWRIGHT_RESTART_UNAVAILABLE_URL;
  const errorURL = process.env.PLAYWRIGHT_RESTART_ERROR_URL;
  expect(successURL).toBeTruthy();
  expect(unavailableURL).toBeTruthy();
  expect(errorURL).toBeTruthy();

  await page.goto(successURL!);
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page), { timeout: 20_000 }).toContain("Ready to code");
  await page.keyboard.type("/restart");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Restart local agent server");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 20_000 }).toContain("Local agent server restarted.");

  await openTerminal(page, unavailableURL!);
  await page.keyboard.type("/restart");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Restart is unavailable");

  await openTerminal(page, errorURL!);
  await page.keyboard.type("/restart");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Restart local agent server");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 20_000 }).toContain("restart failed");
});

test("restores and synchronizes a thread approval mode across terminal restarts", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_PERSIST_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url!);
  await expect.poll(() => terminalText(page)).toContain("•  YOLO");

  await page.keyboard.press("Shift+Tab");
  await expect.poll(() => terminalText(page)).toContain("manual");

  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  let text = await terminalText(page);
  expect(text).toContain("manual");
	expect(text).not.toContain("•  Auto");

  await page.keyboard.press("Control+t");
	await expect.poll(() => terminalText(page)).toContain("•  Auto");
  await page.keyboard.press("Shift+Tab");
  await expect.poll(() => terminalText(page)).toContain("•  YOLO");

  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  text = await terminalText(page);
  expect(text).toContain("•  YOLO");
	expect(text).not.toContain("•  Auto");
});

test("fails closed to Manual for an invalid persisted approval mode", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_APPROVAL_INVALID_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url!);

  const text = await terminalText(page);
  expect(text).toContain("manual");
  expect(text).toContain("Could not restore approval mode; using Manual");
  expect(text).toContain("stored mode is invalid");
	expect(text).not.toContain("•  Auto");
});

test("copies the latest finished assistant message through the browser clipboard bridge", async ({ context, page }) => {
  const url = process.env.PLAYWRIGHT_DIRECT_RESUME_URL;
  expect(url).toBeTruthy();
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await openTerminal(page, url);

  await page.keyboard.type("/copy");
  await page.keyboard.press("Enter");

  await expect.poll(() => terminalText(page)).toContain("Copied latest response to clipboard.");
  await expect(page.locator("html")).toHaveAttribute("data-clipboard-state", "copied");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("Newer browser answer");
});

test("does not execute clipboard controls from assistant or tool output", async ({ context, page }) => {
  const url = process.env.PLAYWRIGHT_HOSTILE_URL;
  expect(url).toBeTruthy();
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto(new URL("/clipboard-seed", url).href);
  await page.evaluate(() => navigator.clipboard.writeText("clipboard-sentinel"));
  await openTerminal(page, url);

  const text = await terminalText(page);
  expect(text).toContain("Assistant before <U+001B CONTROL>");
  expect(text).toContain("Tool before <U+001B CONTROL>");
  expect(text).toContain("Assistant second line");
  expect(text).toContain("Tool second line");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("clipboard-sentinel");
  await expect(page.locator("html")).not.toHaveAttribute("data-clipboard-state", "copied");
  await expect(page.locator("html")).not.toHaveAttribute("data-open-url-state", "opened");

  await page.keyboard.type("/copy");
  await page.keyboard.press("Enter");
  await expect(page.locator("html")).toHaveAttribute("data-clipboard-state", "copied");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("Latest safe assistant");
});

test("reports when there is no assistant message to copy", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("/copy");
  await page.keyboard.press("Enter");

  await expect.poll(() => terminalText(page)).toContain("No message to copy yet.");
  await expect(page.locator("html")).not.toHaveAttribute("data-clipboard-state", "copied");
});

test("opens documentation, changelog, and feedback links through the browser terminal", async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(window, "open", {
      configurable: true,
      value: () => null
    });
  });
  await openTerminal(page);

  for (const [command, target] of [
    ["/docs", "https://github.com/semistrict/dago#readme"],
    ["/changelog", "https://github.com/semistrict/dago/releases"],
    ["/feedback", "https://github.com/semistrict/dago/issues/new/choose"]
  ] as const) {
    await page.keyboard.type(command);
    await page.keyboard.press("Enter");
    await expect(page.locator("html")).toHaveAttribute("data-open-url-state", "opened");
    await expect(page.locator("html")).toHaveAttribute("data-opened-url", target);
    await expect.poll(() => terminalText(page)).toContain(target);
  }
});

test("accepts keyboard input and renders local slash-command output", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("/help");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("/plugins  •  Manage plugins");
  await expect.poll(() => terminalText(page)).toContain("/reload  •  Reload environment and config");
  await expect.poll(() => terminalText(page)).toContain("/quit (/q)  •  Exit app");
  expect(await terminalText(page)).not.toContain("/debug-error");

  await page.keyboard.type("/tools");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("- write_todos — Create or replace the structured task list");

  await page.keyboard.type("/model");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Select Model");
  await expect.poll(() => terminalText(page)).toContain("GPT-5.6 Terra (openai) (current)");
  await page.keyboard.press("Escape");

  await page.keyboard.type("/version");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("dacode version: development");
  await expect.poll(() => terminalText(page)).toContain("dago (SDK) version: development");
  await expect.poll(() => terminalText(page)).toContain("Go version:");

  await page.keyboard.type("/about");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("> /version");
});

test("opens, filters, copies, clears, and toggles the debug console", async ({ context, page }) => {
  const url = process.env.PLAYWRIGHT_DIAGNOSTICS_URL;
  expect(url).toBeTruthy();
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await openTerminal(page, url);

  await page.keyboard.type("/debug-error");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Server failed to start: RuntimeError");

  await page.keyboard.press("Control+\\");
  await expect.poll(() => terminalText(page)).toContain("Debug Console");
  let text = await terminalText(page);
  expect(text).toContain("Thread");
  expect(text).toContain("Model");
  expect(text).toContain("Server failed to start: RuntimeError");
  expect(text).toContain("Ctrl+L clear");
	await clickTerminalText(page, "Model", 2);
	await expect(page.locator("html")).toHaveAttribute("data-clipboard-state", "copied");
	await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("openai:gpt-5.6-terra");

  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("min:DEBUG");
  await page.keyboard.press("Tab");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Level [min:DEBUG]");
  await page.keyboard.press("Tab");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("[x] Click to copy");
  await page.keyboard.press("Tab");
  await page.keyboard.press("Enter");
  await expect(page.locator("html")).toHaveAttribute("data-clipboard-state", "copied");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toContain("Server failed to start: RuntimeError");

  await page.keyboard.press("Control+L");
  await expect.poll(() => terminalText(page)).not.toContain("Server failed to start: RuntimeError");
  await page.keyboard.press("Control+\\");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");

  await page.keyboard.type("/debug");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Debug Console");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
});

test("renders real subagent fan-out and toggles it with ctrl+g", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_FANOUT_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("show subagent fanout");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 8_000 }).toContain("dynamic subagents");
  await expect.poll(() => terminalText(page)).toContain("general-purpose");

  await page.keyboard.press("Control+G");
  await expect.poll(() => terminalText(page)).toContain("Ctrl+G expand");
  let text = await terminalText(page);
  expect(text).not.toContain("general-purpose");

  await page.keyboard.press("Control+G");
  await expect.poll(() => terminalText(page)).toContain("Ctrl+G collapse");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("2/2 done");
  await expect.poll(() => terminalText(page)).toContain("Fan-out complete.");
});

test("lists and invokes every built-in skill command", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_SKILLS_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("/skills");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Available skills:");
  await expect.poll(() => terminalText(page)).toContain("remember [built-in]");
  await expect.poll(() => terminalText(page)).toContain("skill-creator [built-in]");
  await expect.poll(() => terminalText(page)).toContain("deepagents-thread-inspector [built-in]");
  await expect.poll(() => terminalText(page)).toContain("playwright-external [project-agents]");

  await page.keyboard.type("/remember browser learning");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Remember skill invoked.");
	let skillText = await terminalText(page);
	expect(skillText).toContain("Skill: remember [built-in]");
	expect(skillText).toContain("User request: browser learning");
	expect(skillText).toContain("Capture only a learning");

  await page.keyboard.type("/skill-creator browser workflow");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Skill creator invoked.");

  await page.keyboard.type("/skill:deepagents-thread-inspector latest turn");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Thread inspector invoked.");
  const text = await terminalText(page);
  expect(text).not.toContain("Unknown command:");
});

test("manages plugins, reloads runtime components, and shows hook status", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_PLUGINS_URL;
  const extraCatalog = process.env.PLAYWRIGHT_PLUGIN_EXTRA_CATALOG;
  expect(url).toBeTruthy();
  expect(extraCatalog).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("/plugins");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Plugins");
  await page.keyboard.press("ArrowRight");
  await expect.poll(() => terminalText(page)).toContain("Active Browser Plugin @ browser");
  await expect.poll(() => terminalText(page)).toContain("1 hooks");
  await page.keyboard.press("Escape");

  await page.keyboard.type("show hook status");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("Checking browser plugin");
  await expect.poll(() => terminalText(page), { timeout: 8_000 }).not.toContain("Checking browser plugin");

  await page.keyboard.type("/plugins");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Plugins");
  await expect.poll(() => terminalText(page)).toContain("Available Browser Plugin @ browser");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Plugin installed. Reload pending.");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Reload plugins?");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");

  await page.keyboard.type("/plugins");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Plugins");
  await page.keyboard.press("ArrowRight");
  await expect.poll(() => terminalText(page)).toContain("Installed");
  await expect.poll(() => terminalText(page)).toContain("Active Browser Plugin @ browser");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Plugin disabled. Reload pending.");
  await page.keyboard.press("ArrowRight");
  await expect.poll(() => terminalText(page)).toContain("Marketplaces");
  await page.keyboard.press("a");
  await expect.poll(() => terminalText(page)).toContain("Marketplace source:");
  await page.keyboard.type(extraCatalog ?? "");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Marketplace added. Reload pending.");
  await expect.poll(() => terminalText(page)).toContain("extra-browser");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("d");
  await expect.poll(() => terminalText(page)).toContain("Remove marketplace extra-browser?");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).not.toContain("Enter remove");
  await page.keyboard.press("d");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Marketplace removed. Reload pending.");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Reload plugins?");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Configuration reloaded.");
  await expect.poll(() => terminalText(page)).toContain("Loaded plugins: available@browser");
  await expect.poll(() => terminalText(page)).toContain("Unloaded plugins: active@browser");

  await page.keyboard.type("/reload");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Plugin state unchanged.");

  await page.keyboard.type("/plugins");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Plugins");
  await page.keyboard.press("ArrowRight");
  await expect.poll(() => terminalText(page)).toContain("Active Browser Plugin @ browser");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Plugin enabled. Reload pending.");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("u");
  await expect.poll(() => terminalText(page)).toContain("Plugin uninstalled. Reload pending.");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Reload plugins?");
  await page.keyboard.press("Escape");
});

test("renders grouped tool lifecycles, inline diffs, streamed markdown, and line-number preferences", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_TRANSCRIPT_URL;
  expect(url).toBeTruthy();
	await page.addInitScript(() => {
		Object.defineProperty(window, "open", {
			configurable: true,
			value: () => null
		});
	});
  await openTerminal(page, url);

	await page.keyboard.type("render transcript tools");
	await page.keyboard.press("Enter");
	await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Completed 2 file writes");
	await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Transcript rendering");
	expect(await terminalText(page)).not.toContain("streamed markdown with inline code");
	let text = await terminalText(page);
	expect(text).toContain("Glamour list item");
	expect(text).toContain("Glamour link (https://example.com/glamour)");
	expect(text).toContain("Renderer");
	expect(text).toContain("State");
	expect(text).toContain("active");
	expect(text).not.toContain("[Glamour link](");
	expect(text).not.toContain("| --- | --- |");
	await clickTerminalText(page, "Glamour link", 2);
	await expect(page.locator("html")).toHaveAttribute("data-opened-url", "https://example.com/glamour");
	expect(text).toContain("Ctrl+O toggle");
	expect(text).not.toMatch(/Changed .*\/transcript-one\.txt/);
	await page.keyboard.press("Control+o");
	await expect.poll(() => terminalText(page)).toMatch(/Changed .*\/transcript-one\.txt/);
	text = await terminalText(page);
	expect(text).toContain("✓ write_file completed");
	expect(text).toMatch(/\s+1 \+ alpha/);
	await expect.poll(() => terminalText(page)).toContain("streamed markdown with inline code");

	await page.keyboard.type("/line-numbers");
	await page.keyboard.press("Enter");
	await expect.poll(() => terminalText(page)).toContain("Diff line numbers hidden for new diffs.");
	await page.keyboard.type("render second diff");
	await page.keyboard.press("Enter");
	await expect.poll(() => terminalText(page), { timeout: 10_000 }).toMatch(/Changed .*\/transcript-second\.txt/);
	text = await terminalText(page);
	const gamma = text.split("\n").find((line) => line.includes("+ gamma"));
	expect(gamma).toBeDefined();
	expect(gamma).not.toMatch(/\d+\s+\+ gamma/);
});

test("collapses a long user message and toggles the full unit with ctrl+o", async ({ page }) => {
  await openTerminal(page);
	const message = `long transcript user ${"界".repeat(10_050)}`;
	await page.keyboard.insertText(message);
	await page.keyboard.press("Enter");
	await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Long user message received.");
	expect(await pageUpUntil(page, "Ctrl+O to show full message")).toContain("Ctrl+O to show full message");
	await page.keyboard.press("Control+o");
	await expect.poll(() => terminalText(page)).not.toContain("Ctrl+O to show full message");
});

test("virtualizes restored transcript history and hydrates it on page up", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_TRANSCRIPT_HISTORY_URL;
  expect(url).toBeTruthy();
	await page.goto(url!);
	await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
	await expect.poll(() => terminalText(page)).toContain("Virtual history assistant 094");
	expect(await terminalText(page)).not.toContain("Virtual history user 000");
	const hydrated = await pageUpUntil(page, "Virtual history user 000");
	expect(hydrated).toContain("Virtual history user 000");
	expect(await terminalText(page)).not.toContain("earlier transcript items virtualized");
});

test("denies or persists exact trust for an external skill symlink", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_SKILLS_URL;
  expect(url).toBeTruthy();
  await openTerminal(page, url);

  await page.keyboard.type("/skill:playwright-external verify target");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Trust external skill?");
  let text = await terminalText(page);
  expect(text).toContain("Only approve if you recognize and trust this directory.");
  expect(text).toContain("N/Esc to cancel");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Skill invocation cancelled; no trust was granted.");
  expect(await terminalText(page)).not.toContain("External skill invoked.");

  await page.keyboard.type("/skill:playwright-external verify target");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Trust external skill?");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("External skill invoked.");

  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  await page.keyboard.type("/skill:playwright-external trusted retry");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("External skill invoked.");
  text = await terminalText(page);
  expect(text).not.toContain("Trust external skill?");
});

test("edits and cancels composer drafts through the configured external editor", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_EDITOR_URL);

	await page.keyboard.type("/help");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("/editor");
  await expect.poll(() => terminalText(page)).toContain("Ctrl+X open draft in editorfixture");

  await page.keyboard.type("/editor");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("> edited by fixture");

  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");
  await page.keyboard.type("draft from terminal");
  await page.keyboard.press("Control+X");
  await expect.poll(() => terminalText(page)).toContain("> edited: draft from terminal");

  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");
  await page.keyboard.type("cancel edit");
  await page.keyboard.press("Control+X");
  await expect.poll(() => terminalText(page)).toContain("> cancel edit");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
});

test("selects, validates, persists, and clears reasoning effort", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_EFFORT_URL);

  await page.keyboard.type("/effort");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Select Reasoning Effort");
  await expect.poll(() => terminalText(page)).toContain("openai:gpt-5.6-terra");
  await expect.poll(() => terminalText(page)).toContain("› none");
  await page.keyboard.press("Tab");
  await expect.poll(() => terminalText(page)).toContain("› low");
  await page.keyboard.press("ArrowUp");
  await expect.poll(() => terminalText(page)).toContain("› none");
  await page.keyboard.press("Tab");
  await page.keyboard.press("Tab");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Reasoning effort for openai:gpt-5.6-terra set to medium.");
  await expect.poll(() => terminalText(page)).toContain("openai:gpt-5.6-terra medium");

  await page.keyboard.type("/effort");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("medium (current)");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).not.toContain("Select Reasoning Effort");

  await page.keyboard.type("/effort HIGH");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("set to high.");
  await expect.poll(() => terminalText(page)).toContain("openai:gpt-5.6-terra high");

  await page.keyboard.type("/effort impossible");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Unsupported reasoning effort \"impossible\"");
  await expect.poll(() => terminalText(page)).toContain("Supported efforts: none, low, medium, high, xhigh");

  await page.keyboard.type("/effort low");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("set to low.");
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("openai:gpt-5.6-terra low");

  await page.keyboard.type("/effort clear");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Reasoning effort override cleared for openai:gpt-5.6-terra.");
});

test("explains when reasoning effort is unavailable for the active model", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_UNSUPPORTED_EFFORT_URL);

  await page.keyboard.type("/effort");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Reasoning effort is not configurable for fixture:plain-model.");

  await page.keyboard.type("/effort high");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("/effort high");
  await expect.poll(() => terminalText(page)).toContain("Reasoning effort is not configurable for fixture:plain-model.");

  await page.keyboard.type("/effort clear");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("No reasoning effort override was set for fixture:plain-model.");
});

test("toggles message timestamps and the overflowing chat scrollbar", async ({ page }) => {
  await page.setViewportSize({ width: 800, height: 360 });
  await openTerminal(page);

  await page.keyboard.type("show the display toggles");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Thinking…");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).not.toContain("Thinking…");
  expect(await terminalText(page)).not.toMatch(/\b\d{2}:\d{2}:\d{2}\b/);

  await page.keyboard.type("/timestamps");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Message timestamps shown.");
  await expect.poll(() => terminalText(page)).toMatch(/\b\d{2}:\d{2}:\d{2}\b/);

  await page.keyboard.type("/timestamps");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Message timestamps hidden.");
  await expect.poll(() => terminalText(page)).not.toMatch(/\b\d{2}:\d{2}:\d{2}\b/);

  for (let index = 0; index < 8; index += 1) {
    await page.keyboard.type("/version");
    await page.keyboard.press("Enter");
  }
  expect(await terminalText(page)).not.toContain("█");

  await page.keyboard.type("/scrollbar");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Chat scrollbar shown.");
  await expect.poll(() => terminalText(page)).toContain("█");

  await page.keyboard.type("/scrollbar");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Chat scrollbar hidden.");
  await expect.poll(() => terminalText(page)).not.toContain("█");
});

test("shows context, token usage, and thread cost", async ({ page }) => {
  await openTerminal(page);

  await page.keyboard.type("show usage commands");
  await page.keyboard.press("Enter");
	await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Tokens: 1");

  await page.keyboard.type("/context");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Context • openai:gpt-5.6-terra");
  await expect.poll(() => terminalText(page)).toContain("1 / 128k");
  await expect.poll(() => terminalText(page)).toContain("Used context");
  await expect.poll(() => terminalText(page)).toContain("Free space");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");

  await page.keyboard.type("/tokens");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("1 / 128k tokens (0.0%) · openai:gpt-5.6-terra");
  await expect.poll(() => terminalText(page)).toContain("Input: 1");

  await page.keyboard.type("/cost");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Estimated thread cost: $0.0012");
  await expect.poll(() => terminalText(page)).toContain("1 recorded request • 1 input • 0 output tokens");
  await expect.poll(() => terminalText(page)).toContain("By model");
  await expect.poll(() => terminalText(page)).toContain("openai:gpt-5.6-terra");
  await expect.poll(() => terminalText(page)).toContain("By purpose");
  await expect.poll(() => terminalText(page)).toContain("Assistant");

  await page.keyboard.type("/quit");
  await page.keyboard.press("Enter");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "closed");
  await expect.poll(() => terminalText(page)).toContain("Session usage");
  await expect.poll(() => terminalText(page)).toContain("Estimated thread cost: $0.0012");
});

test("opens startup goal criteria in the review panel", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_GOAL_STARTUP_URL);
  await expect.poll(() => terminalText(page)).toContain("Review goal criteria");
  await expect.poll(() => terminalText(page)).toContain("Finish the release checklist");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Goal proposal cancelled.");
});

test("reviews, accepts, displays, and pauses a goal", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_GOAL_RUBRIC_URL);

  await page.keyboard.type("/goal Finish the release checklist");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Review goal criteria");
  await expect.poll(() => terminalText(page)).toContain("Release checklist is complete.");
  await expect.poll(() => terminalText(page)).toContain("1. Accept proposed criteria (y)");
  await page.keyboard.press("y");
  await expect.poll(() => terminalText(page)).toContain("Goal accepted. Finish the release checklist");
  await expect.poll(() => terminalText(page)).toContain("Goal • active");
  await expect.poll(() => terminalText(page)).toContain("rubric");
  await expect.poll(() => terminalText(page)).toContain("Continuing goal…");

  await page.keyboard.press("Control+c");
  await expect.poll(() => terminalText(page)).toContain("Operation cancelled.");

  await page.keyboard.type("/goal pause");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Goal paused.");
  await expect.poll(() => terminalText(page)).toContain("Goal • paused");

  await page.keyboard.type("/goal show");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Criteria:");
});

test("keeps the model system prompt cache-stable across an active-goal tool loop", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_GOAL_RUBRIC_URL);

  await page.keyboard.type("/goal Prove active goal cache stability");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Review goal criteria");
  await expect.poll(() => terminalText(page)).toContain("Prove active goal cache stability");
  await page.keyboard.press("y");
  await expect.poll(() => terminalText(page)).toContain("Goal accepted. Prove active goal cache stability");
  await expect.poll(() => terminalText(page), { timeout: 15_000 }).toContain("Active-goal system prompt remained cache-stable.");
  await expect.poll(() => terminalText(page)).not.toContain("Active-goal system prompt changed between model calls.");
});

test("keeps the local-context system prompt cache-stable across turns after workspace changes", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_TRANSCRIPT_URL);

  await page.keyboard.type("create a local context cache probe");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 15_000 }).toContain("Local context cache probe prepared.");

  await page.keyboard.type("verify the local context cache snapshot");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 15_000 }).toContain("Local-context system prompt remained cache-stable across turns.");
  await expect.poll(() => terminalText(page)).not.toContain("Local-context system prompt changed across turns.");
});

test("edits goal criteria with the external editor", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_GOAL_RUBRIC_URL);
  await page.keyboard.type("/clear");
  await page.keyboard.press("Enter");

  await page.keyboard.type("/goal Finish the release checklist");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Review goal criteria");
  await page.keyboard.press("e");
  await expect.poll(() => terminalText(page)).toContain("Ctrl+X editorfixture");
  await page.keyboard.press("Control+x");
  await expect.poll(() => terminalText(page)).toContain("edited: - Release checklist is complete.");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Goal accepted.");
  await expect.poll(() => terminalText(page)).toContain("edited: - Release checklist is complete.");
  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Operation cancelled.");
});

test("validates goal rejection feedback, regenerates criteria, and cancels", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_GOAL_RUBRIC_URL);
  await page.keyboard.type("/clear");
  await page.keyboard.press("Enter");

  await page.keyboard.type("/goal Finish the release checklist");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Review goal criteria");
  await page.keyboard.press("r");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Enter some feedback, or press Esc to go back.");
  await page.keyboard.type("make the browser check explicit");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Browser verification passes.");
  await page.keyboard.press("n");
  await expect.poll(() => terminalText(page)).toContain("Goal proposal cancelled.");
});

test("configures persistent, file, and one-turn rubrics", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_GOAL_RUBRIC_URL);
  await page.keyboard.type("/clear");
  await page.keyboard.press("Enter");

  await page.keyboard.type("/criteria set verification passes");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Rubric set.");
  await expect.poll(() => terminalText(page)).toContain("rubric");

  await page.keyboard.type("/rubric show");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Grader: openai:gpt-5.6-terra · max iterations: 3");

  await page.keyboard.type("/rubric model openai:gpt-5.6-terra");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Grader model set to openai:gpt-5.6-terra.");
  await page.keyboard.type("/rubric max-iterations 2");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Grader max iterations set to 2.");

  await page.keyboard.type("/rubric clear");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Rubric cleared.");
  await page.keyboard.type("/rubric file release-rubric.md");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Rubric set from file.");

  await page.keyboard.type("/rubric next release checklist is complete");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Rubric set for next turn.");
  await page.keyboard.type("verify the release");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Latest result: satisfied");
  await expect.poll(() => terminalText(page)).toContain("All acceptance criteria passed.");
  await expect.poll(() => terminalText(page)).toContain("✓ Release checklist is complete.");
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
  await expect.poll(() => terminalText(page)).toContain("/plugins  •  Manage plugins");
  await page.keyboard.type("/clear");
  await page.keyboard.press("Enter");

  await expect.poll(async () => shortThreadID(await terminalText(page))).not.toBe(originalThread);
  await expect.poll(() => terminalText(page)).not.toContain("/plugins  •  Manage plugins");
});

test("closes the browser session with the local quit command", async ({ page }) => {
  await openTerminal(page);
	const threadID = fullThreadID(await terminalText(page));
	expect(threadID).toBeDefined();
	await page.keyboard.type("save this session before slash quit");
	await page.keyboard.press("Enter");
	await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Thinking…");
	await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("> What would you like to build?");

  await page.keyboard.type("/quit");
  await page.keyboard.press("Enter");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "closed");
  await expect(page.getByRole("status")).toHaveText("Session ended");
	await expect.poll(() => terminalText(page)).toContain(`Resume this session:\ndacode resume ${threadID}`);
});

test("closes the browser session cleanly on ctrl+d", async ({ page }) => {
  await openTerminal(page);
	const threadID = fullThreadID(await terminalText(page));
	expect(threadID).toBeDefined();
	await page.keyboard.type("save this session before ctrl+d");
	await page.keyboard.press("Enter");
	await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Thinking…");
	await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("> What would you like to build?");

  await page.keyboard.press("Control+d");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "closed");
  await expect(page.getByRole("status")).toHaveText("Session ended");
	await expect.poll(() => terminalText(page)).toContain(`Resume this session:\ndacode resume ${threadID}`);
});

test("supports command and file autocomplete with visible input modes", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_INPUT_URL);

  await page.keyboard.type("/");
  await expect.poll(() => terminalText(page)).toContain("› /agents");
	await page.keyboard.type("rem");
	await expect.poll(() => terminalText(page)).toContain("› /remember");
	await page.keyboard.press("Backspace");
	await page.keyboard.press("Backspace");
	await page.keyboard.press("Backspace");
  await page.keyboard.type("he");
  await expect.poll(() => terminalText(page)).toContain("› /help");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("/plugins  •  Manage plugins");

  await page.keyboard.type("inspect @mention");
  await expect.poll(() => terminalText(page)).toContain("mention-target.txt");
  await page.keyboard.press("Tab");
  await expect.poll(() => terminalText(page)).toContain("@mention-target.txt");

  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");
  await page.keyboard.type("!");
  await expect.poll(() => terminalText(page)).toContain("$ What would you like to build?");
  await page.keyboard.type("!");
  await expect.poll(() => terminalText(page)).toContain("$ What would you like to build?");
  await page.keyboard.press("Backspace");
  await expect.poll(() => terminalText(page)).toContain("> What would you like to build?");
});

test("collapses modern and legacy large pastes and recognizes dropped paths", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_INPUT_URL);

  const modern = `${"modern-paste ".repeat(90)}modern-paste-tail`;
  await page.evaluate((value) => window.dacodeTerminal?.paste(value), modern);
  await expect.poll(() => terminalText(page)).toContain("[Pasted text #1]");
  await page.keyboard.press("Control+c");

  const legacy = `${"legacy-paste ".repeat(90)}legacy-paste-tail`;
  await page.evaluate((value) => window.dacodeTerminal?.input(value, true), legacy);
  await expect.poll(() => terminalText(page)).toContain("[Pasted text #2]");
  await page.keyboard.press("Control+c");

  await page.evaluate(() => window.dacodeTerminal?.paste("mention-target.txt"));
  await expect.poll(() => terminalText(page)).toContain("@mention-target.txt");
  await page.keyboard.press("Control+c");
  await page.evaluate(() => {
    const transfer = new DataTransfer();
    transfer.setData("text/plain", "screen.png");
    document.querySelector("#terminal")?.dispatchEvent(new DragEvent("drop", { bubbles: true, dataTransfer: transfer }));
  });
  await expect.poll(() => terminalText(page)).toContain("[image 1]");
});

test("persists input history across terminal restarts", async ({ page }) => {
  const url = process.env.PLAYWRIGHT_INPUT_URL;
  await openTerminal(page, url);
  const historical = "persistent browser history 9182";
  await page.keyboard.type(historical);
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain(historical);
  await expect.poll(() => terminalText(page), { timeout: 5_000 }).toContain("Ready");

  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
  await page.keyboard.press("ArrowUp");
  await expect.poll(() => terminalText(page)).toContain(`> ${historical}`);
});

test("runs shared and incognito shell commands and drains queued messages", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_INPUT_URL);

  await page.keyboard.type("!printf shared-shell-output");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("shared-shell-output");
  await page.keyboard.type("!!printf private-shell-output");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).not.toContain("$ printf private-shell-output");
  await expect.poll(() => terminalText(page)).toContain("private-shell-output");

  await page.keyboard.type("first slow request 9182");
  await page.keyboard.press("Enter");
  await page.keyboard.type("second queued request 9182");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Queued input #1.");
  await expect.poll(() => terminalText(page), { timeout: 7_000 }).toContain("second queued request 9182");
});

test("does not render clear or clipboard buttons for a draft", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_INPUT_URL);

  await page.keyboard.type("clear button draft");
  await expect.poll(() => terminalText(page)).toContain("clear button draft");
  const text = await terminalText(page);
  expect(text).not.toContain("[ X ] clear");
  expect(text).not.toContain("[ COPY ] copy");
});

test("handles terminal newline, deletion, space, and lock-key quirks", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_INPUT_URL);

  await page.keyboard.type("alpha beta");
  await page.keyboard.press("Control+Backspace");
  await expect.poll(() => terminalText(page)).toContain("> alpha ");
  await page.keyboard.press("CapsLock");
  await page.keyboard.type("gamma");
  await page.keyboard.press("Shift+Enter");
  await page.keyboard.type("delta");
  await expect.poll(() => terminalText(page)).toContain("delta");
  await page.evaluate(() => window.dacodeTerminal?.input("\u001b[32u", true));
  await page.keyboard.type("epsilon");
  await expect.poll(() => terminalText(page)).toContain(" epsilon");
  await page.keyboard.type("\\");
  await page.keyboard.press("Enter");
  await page.keyboard.type("zeta");
  await expect.poll(() => terminalText(page)).toContain("zeta");
});

test("copies terminal selections and suppresses the first refocus click", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await openTerminal(page, process.env.PLAYWRIGHT_INPUT_URL);

  await page.evaluate(() => {
    const terminal = window.dacodeTerminal;
    if (!terminal) return;
    const buffer = terminal.buffer.active;
    for (let row = 0; row < buffer.length; row += 1) {
      const line = buffer.getLine(row)?.translateToString(true) ?? "";
      const column = line.indexOf("Ready to code");
      if (column >= 0) {
        terminal.select(column, row, "Ready to code".length);
        break;
      }
    }
  });
  await page.keyboard.press("Control+Shift+c");
  await expect(page.locator("html")).toHaveAttribute("data-clipboard-state", "copied");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("Ready to code");

  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await page.locator("#terminal").click({ position: { x: 40, y: 40 } });
  await expect(page.locator("html")).toHaveAttribute("data-refocus-click", "suppressed");
});

test("answers required interactive questions without exposing answers in the collapsed row", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_ASK_USER_URL);

  await page.keyboard.type("show ask user browser flow");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 8_000 }).toContain("Agent has 3 Questions for you");
  const prompt = await terminalText(page);
  expect(prompt).toContain("Project name? (required)");
  expect(prompt).not.toContain("Project name? (required) (required)");
  expect(prompt).toContain("Other (type your answer)");
  expect(prompt).toContain("Tab/Shift+Tab switch question");
  expect(prompt).toContain("Ctrl+X edit in editorfixture");

  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain(
    "Please provide an answer to all questions before continuing."
  );
  const privateAnswer = "private-answer-4815";
  await page.keyboard.type(privateAnswer);
  await page.keyboard.press("Control+j");
  await page.keyboard.type("second line");
  await page.keyboard.press("Control+x");
  await expect.poll(() => terminalText(page)).toContain("edited: private-answer-4815");
  await page.keyboard.press("Enter");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await page.keyboard.press("Enter");

  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Answers received.");
  await expect.poll(() => terminalText(page)).toContain("User answered");
  const completed = await terminalText(page);
  expect(completed).not.toContain(privateAnswer);
  expect(completed).not.toContain("second line");
});

test("navigates questions and submits a custom multiple-choice answer", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_ASK_USER_URL);

  await page.keyboard.type("show ask user browser flow");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 8_000 }).toContain("Agent has 3 Questions for you");
  await page.keyboard.press("Tab");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.type("green custom answer");
  await page.keyboard.press("Shift+Tab");
  await page.keyboard.type("navigation project");
  await page.keyboard.press("Enter");
  await page.keyboard.press("Enter");
  await page.keyboard.press("Enter");

  await expect.poll(() => terminalText(page), { timeout: 10_000 }).toContain("Answers received.");
  const completed = await terminalText(page);
  expect(completed).toContain("User answered");
  expect(completed).not.toContain("green custom answer");
  expect(completed).not.toContain("navigation project");
});

test("dismisses an interactive question without resuming the run", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_ASK_USER_CANCEL_URL);

  await page.keyboard.type("show ask user browser flow");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 8_000 }).toContain("Agent has 3 Questions for you");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Question dismissed.");
  const dismissed = await terminalText(page);
  expect(dismissed).toContain("✗ ask_user");
  expect(dismissed).not.toContain("Answers received.");
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
  const initialLine = (await terminalText(page)).match(/browser-check\s+Inspect\s+(\d+)s\s+•\s+~([\d.]+)(k?) tok/);
  const initialElapsed = Number(initialLine?.[1] ?? -1);
  const initialTokens = Number(initialLine?.[2] ?? 0) * (initialLine?.[3] === "k" ? 1_000 : 1);
  expect(initialElapsed).toBeGreaterThanOrEqual(0);
  expect(initialTokens).toBeGreaterThan(0);
  await page.waitForTimeout(1_100);
  const laterElapsed = Number((await terminalText(page)).match(/browser-check\s+Inspect\s+(\d+)s\s+•\s+~[\d.]+k? tok/)?.[1] ?? -1);
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
    const match = (await terminalText(page)).match(/token-progress-worker\s+Inspect\s+\d+s\s+•\s+~([\d.]+)(k?) tok/);
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
		structuredCorrections: 2,
    workerExecuteCalls: 8,
    workerExecuteContinuations: 7
  });
  expect(fixture.approvalReviews).toBeGreaterThanOrEqual(8);
  expect(fixture.failedWorkerRequests).toBeGreaterThanOrEqual(1);

  await page.keyboard.type("/workflows");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("WORKFLOW CONTROL");
  const text = await terminalText(page);
  expect(text).toContain("SUCCESS");
  expect(text).toContain("7 done • 0 active • 2 failed");
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

test("local tools use the real working directory instead of a virtual slash root", async ({ page, request }) => {
  test.setTimeout(60_000);
  const url = process.env.PLAYWRIGHT_WORKFLOW_FAKE_URL;
  const apiURL = process.env.PLAYWRIGHT_WORKFLOW_FAKE_API_URL;
  expect(url).toBeTruthy();
  expect(apiURL).toBeTruthy();
  const beforeResponse = await request.get(`${apiURL}/fixture-state`);
  const before = await beforeResponse.json();

  await openTerminal(page, url);
  await page.keyboard.type("LOCAL_PATH_SEMANTICS: inspect only the current repository root.");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page), { timeout: 20_000 }).toContain("Local working-directory paths verified.");

  const text = await terminalText(page);
  expect(text).not.toContain("Automatic review denied execute");
  const afterResponse = await request.get(`${apiURL}/fixture-state`);
  const after = await afterResponse.json();
  expect(after.localPathPrompts).toBe(before.localPathPrompts + 1);
  expect(after.localPathExecutions).toBe(before.localPathExecutions + 1);
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
  expect(text).toMatch(/[1-9]\d* done • 0 active • 0 failed/);
  expect(text).not.toContain("no agent calls");
});
