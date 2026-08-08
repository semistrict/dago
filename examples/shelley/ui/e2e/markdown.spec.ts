import { test, expect } from "@playwright/test";
import { createConversationViaAPI } from "./helpers";

// All tests create the conversation via the API and then navigate directly to
// it. This avoids the SSE subscribe-vs-publish race that occurs when the
// browser opens a brand-new conversation while the first turn is still being
// recorded (see helpers.ts), which otherwise flakes waitForSelector(".message-agent").
// Split out of the original markdown.spec.ts, which at 47s of test time was
// one of the specs gating the playwright shards (see
// .buildkite/steps/shelley-playwright-shard.py -- files are the sharding unit,
// so a single file cannot be spread across lanes). The two halves are
// independent: every test creates its own conversation via the API.
test.describe("Markdown rendering", () => {
  test("renders markdown formatting in agent messages", async ({ page, request }) => {
    const slug = await createConversationViaAPI(
      request,
      "markdown: **bold** and *italic* and `code`",
    );
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const agent = page.locator(".message-agent").last();
    // Markdown should be rendered as HTML elements
    await expect(agent.locator("strong")).toContainText("bold", { timeout: 30000 });
    await expect(agent.locator("em")).toContainText("italic");
    await expect(agent.locator("code")).toContainText("code");
  });

  test("renders local images via the per-message file endpoint", async ({ page, request }) => {
    // The "inline image" predictable pattern writes a tiny PNG into the
    // conversation cwd (/tmp) via bash, then references it with relative-path
    // markdown. The UI should rewrite the src to /api/message/{id}/file and
    // load the bytes from the server.
    const slug = await createConversationViaAPI(request, "inline image", { agentTimeout: 60000 });
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const img = page.locator(".message-agent img").last();
    await expect(img).toBeVisible({ timeout: 30000 });
    const src = await img.getAttribute("src");
    expect(src).toMatch(/^\/api\/message\/[^/]+\/file\?path=/);

    // The browser should successfully fetch the image bytes (naturalWidth > 0
    // only when the image actually loaded).
    await expect
      .poll(async () => img.evaluate((el: HTMLImageElement) => el.naturalWidth), {
        timeout: 15000,
      })
      .toBeGreaterThan(0);
  });

  test("markdown links open in new tab with noopener", async ({ page, request }) => {
    const slug = await createConversationViaAPI(
      request,
      "markdown: [example](https://example.com)",
    );
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const link = page.locator(".message-agent").last().locator("a").first();
    await expect(link).toHaveAttribute("href", "https://example.com", { timeout: 30000 });
    await expect(link).toHaveAttribute("target", "_blank");
    await expect(link).toHaveAttribute("rel", "noopener noreferrer");
  });

  test("user messages never render markdown", async ({ page, request }) => {
    // Send a message with markdown syntax - user messages should show raw text
    const slug = await createConversationViaAPI(request, "**bold** and *italic*");
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const user = page.locator(".message-user").last();
    // The raw markdown characters should be visible
    await expect(user).toContainText("**bold**", { timeout: 30000 });
    // User message should NOT have <strong> or <em> — should be plain text
    expect(await user.locator("strong").count()).toBe(0);
    expect(await user.locator("em").count()).toBe(0);
  });

  test("coalesces web-search citation blocks into one paragraph with markers", async ({
    page,
    request,
  }) => {
    // The "web search" predictable pattern returns a server-side web-search
    // message: a server_tool_use block, a web_search_tool_result, and many
    // small text blocks where cited quotes carry a Citations array. The UI must
    // merge adjacent text blocks (no stray line breaks) and surface inline
    // citation markers + a numbered Sources list.
    const slug = await createConversationViaAPI(request, "web search");
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const agent = page.locator(".message-agent").last();
    const content = agent.locator('[data-testid="message-content"]');
    await expect(content).toContainText("mid-session model switching", { timeout: 30000 });

    // The sentence that used to be split across blocks now reads continuously
    // (an inline citation marker may sit between the two halves).
    await expect(content).toContainText(/never lose work.*so model switching pairs well/s);

    // Inline citation markers render as superscript source links.
    expect(await content.locator("sup.citation-refs a.citation-ref").count()).toBeGreaterThan(0);

    // A numbered Sources list is appended for the cited run.
    const sources = content.locator("ol.citation-sources li.citation-source");
    expect(await sources.count()).toBeGreaterThan(0);
    await expect(sources.first().locator("a")).toHaveAttribute("href", /^https?:\/\//);
  });
});
