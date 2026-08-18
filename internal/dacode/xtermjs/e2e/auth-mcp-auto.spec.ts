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

async function openTerminal(page: Page, url: string | undefined): Promise<void> {
  expect(url).toBeTruthy();
  await page.goto(url ?? "/");
  await expect(page.locator("html")).toHaveAttribute("data-terminal-state", "connected");
  await expect.poll(() => terminalText(page)).toContain("Ready to code");
}

async function runCommand(page: Page, command: string): Promise<void> {
  await page.waitForTimeout(100);
  await page.keyboard.type(command);
  await page.keyboard.press("Enter");
  await page.waitForTimeout(100);
}

async function selectRow(page: Page, label: string): Promise<void> {
  for (let index = 0; index < 140; index += 1) {
    const text = await visibleTerminalText(page);
    if (text.split("\n").some((line) => line.trimStart().startsWith("> ") && line.includes(label))) return;
    await page.keyboard.press("ArrowDown");
    await page.waitForTimeout(20);
  }
  throw new Error(`could not select terminal row ${label}`);
}

async function openAuthProvider(page: Page, provider: string): Promise<void> {
  await runCommand(page, "/auth");
  await expect.poll(() => visibleTerminalText(page)).toContain("Manage credentials");
  await selectRow(page, provider);
  await page.keyboard.press("Enter");
}

test("manages API keys through /auth and the /connect alias without exposing input", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_AUTH_MCP_AUTO_URL);
  await openAuthProvider(page, "Anthropic");
  await expect.poll(() => terminalText(page)).toContain("Add API key");
  await page.keyboard.type("browser-value-123");
  await expect.poll(() => terminalText(page)).toContain("API key: <hidden>");
  expect(await terminalText(page)).not.toContain("browser-value-123");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Credential saved. Restart or reload");
  expect(await terminalText(page)).not.toContain("browser-value-123");

  await page.keyboard.press("Delete");
  await expect.poll(() => terminalText(page)).toContain("Remove credential?");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Stored credential removed");
  await page.keyboard.press("Escape");
  await runCommand(page, "/connect");
  await expect.poll(() => terminalText(page)).toContain("Manage credentials");
});

test("shows subscription sign-in success and sanitized failure", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_AUTH_MCP_AUTO_URL);
  await openAuthProvider(page, "OpenAI Subscription");
  await expect.poll(() => visibleTerminalText(page)).toContain("OpenAI subscription sign-in");
  await expect.poll(() => visibleTerminalText(page)).toContain("Subscription sign-in complete");
  expect(await terminalText(page)).not.toContain("access_token");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("Signed in with an OpenAI subscription");

  const errorPage = await page.context().newPage();
  await openTerminal(errorPage, process.env.PLAYWRIGHT_AUTH_ERROR_URL);
  await openAuthProvider(errorPage, "OpenAI Subscription");
  await expect.poll(() => visibleTerminalText(errorPage)).toContain("Sign-in failed. Try again.");
  expect(await terminalText(errorPage)).not.toContain("fixture provider secret");
});

test("cancels subscription sign-in and ignores its late completion", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_AUTH_CANCEL_URL);
  await openAuthProvider(page, "OpenAI Subscription");
  await expect.poll(() => visibleTerminalText(page)).toContain("Waiting for sign-in to finish");
  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("Manage credentials");
  await page.waitForTimeout(100);
  const text = await terminalText(page);
  expect(text).not.toContain("Subscription sign-in complete");
  expect(text).not.toContain("Sign-in failed");
});

test("views MCP tools, error details, reconnects, and applies session disable", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_AUTH_MCP_AUTO_URL);
  await runCommand(page, "/mcp");
  await expect.poll(() => terminalText(page)).toContain("MCP Servers");
  await expect.poll(() => terminalText(page)).toContain("healthy");

  await selectRow(page, "broken");
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleTerminalText(page)).toContain("MCP Server Error: broken");
  await expect.poll(() => terminalText(page)).toContain("bounded fixture connection failure");
  await page.keyboard.press("Escape");
  await expect.poll(() => visibleTerminalText(page)).toContain("MCP Servers");
  await selectRow(page, "healthy");
  await page.keyboard.press("F2");
  await expect.poll(() => visibleTerminalText(page)).toContain("Apply MCP server changes?");
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleTerminalText(page)).toContain("disabled");

  await page.keyboard.press("Escape");
  await expect.poll(() => terminalText(page)).toContain("MCP servers reconnected");
  await runCommand(page, "/mcp reconnect");
  await expect.poll(() => visibleTerminalText(page)).toContain("Apply MCP server changes?");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("MCP servers reconnected");
});

test("handles MCP OAuth success, provider failure, and optional empty workspace", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_AUTH_MCP_AUTO_URL);
  await runCommand(page, "/mcp login oauth-success");
  await expect.poll(() => terminalText(page)).toContain("paste the final callback URL");
  await page.keyboard.type("https://localhost.invalid/callback?code=opaque");
  expect(await terminalText(page)).not.toContain("code=opaque");
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleTerminalText(page)).toContain("Reconnect to load new tools");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("MCP servers reconnected");

  await runCommand(page, "/mcp login oauth-error");
  await expect.poll(() => terminalText(page)).toContain("paste the final callback URL");
  await page.keyboard.type("https://localhost.invalid/callback?code=opaque");
  await page.keyboard.press("Enter");
  await expect.poll(() => terminalText(page)).toContain("MCP login failed");
  expect(await terminalText(page)).not.toContain("fixture provider detail");
  await page.keyboard.press("Enter");

  await runCommand(page, "/mcp login slack-workspace");
  await expect.poll(() => terminalText(page)).toContain("submit an empty value");
  await page.keyboard.press("Enter");
  await expect.poll(() => visibleTerminalText(page)).toContain("Reconnect to load new tools");
  await page.keyboard.press("Escape");
});

test("manages the Auto classifier and renders allow, deny, and human fallback", async ({ page }) => {
  await openTerminal(page, process.env.PLAYWRIGHT_AUTH_MCP_AUTO_URL);
  await runCommand(page, "/auto unknown");
  await expect.poll(() => terminalText(page)).toContain("Current reviewer: openai:gpt-5.6-terra");
  await runCommand(page, "/auto model");
  await expect.poll(() => terminalText(page)).toContain("Choose Auto Classifier Model");
  await page.keyboard.press("Control+s");
  await expect.poll(() => terminalText(page)).toContain("Classifier default saved");

  await runCommand(page, "/auto model fixture:valid");
  await expect.poll(() => terminalText(page)).toContain("Classifier model set to fixture:valid");
  await runCommand(page, "/auto model fixture:invalid");
  await expect.poll(() => terminalText(page)).toContain("Classifier model is unavailable");
  await runCommand(page, "/auto model clear");
  await expect.poll(() => terminalText(page)).toContain("reviews use the main agent model");

  await runCommand(page, "auto allow action");
  await expect.poll(() => terminalText(page)).toContain("Fixture action completed");
  expect(await terminalText(page)).not.toContain("Automatic review approved execute");
  await runCommand(page, "auto deny action");
  await expect.poll(() => terminalText(page)).toContain("Automatic review denied execute");
  await runCommand(page, "auto fallback action");
  await expect.poll(() => terminalText(page)).toContain("Automatic review unavailable; a user decision is required");
  await expect.poll(() => terminalText(page)).toContain("Approve (y)");
  await page.keyboard.press("n");
});
