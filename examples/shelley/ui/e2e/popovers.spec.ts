import { test, expect } from "@playwright/test";
import { createConversationViaAPI } from "./helpers";

// Anchored-popover contract for the two floating popups migrating to PrimeVue
// Popover: the ConversationTOC "Jump to…" panel and the ContextUsageBar token
// popup (opened from the "<tokens> · <model>" status label). These specs pin
// the DOM/ARIA contract (classes, labels, dismissal behavior) so it holds
// across the hand-rolled and PrimeVue implementations.

// A cwd for the readout tests. It has to exist on the machine running the suite
// (the server rejects a missing one) and be long enough to put the readout under
// width pressure at a narrow viewport, which is what the ellipsis test measures.
const READOUT_CWD = "/tmp";

test.describe("Conversation TOC popover", () => {
  test("opens from the nav button, lists entries, and dismisses", async ({ page, request }) => {
    test.setTimeout(60000);
    const slug = await createConversationViaAPI(request, "echo table of contents");
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const tocButton = page.locator(".toc-button");
    await expect(tocButton).toBeVisible({ timeout: 30000 });
    await expect(tocButton).toHaveAttribute("aria-expanded", "false");
    await expect(tocButton).toHaveAccessibleName("Conversation table of contents");

    await tocButton.click();
    const popover = page.locator(".toc-popover");
    await expect(popover).toBeVisible();
    await expect(popover.locator(".toc-popover-list")).toBeVisible();
    await expect(tocButton).toHaveAttribute("aria-expanded", "true");
    await expect(popover.locator(".toc-popover-title")).toHaveText("Jump to…");

    // First/last entries are the fixed top/bottom anchors; the seeded user
    // message appears as a .toc-entry-user row in between.
    const entries = popover.locator(".toc-entry");
    await expect(entries.first()).toContainText("Top of conversation");
    await expect(entries.last()).toContainText("End of conversation");
    await expect(popover.locator(".toc-entry-user").first()).toContainText(
      "echo table of contents",
    );

    // Escape dismisses.
    await page.keyboard.press("Escape");
    await expect(popover).toBeHidden();
    await expect(tocButton).toHaveAttribute("aria-expanded", "false");

    // Outside click dismisses.
    await tocButton.click();
    await expect(popover).toBeVisible();
    await page.locator(".messages-container").click({ position: { x: 10, y: 10 } });
    await expect(popover).toBeHidden();

    // Clicking a user entry closes the popover and records a #m-<short> hash.
    await tocButton.click();
    await expect(popover).toBeVisible();
    await popover.locator(".toc-entry-user").first().click();
    await expect(popover).toBeHidden();
    await expect(async () => {
      expect(new URL(page.url()).hash).toMatch(/^#m-[a-zA-Z0-9]+$/);
    }).toPass({ timeout: 5000 });
  });

  test("shows timeline images as thumbnails", async ({ page, request }) => {
    test.setTimeout(180000);

    const inlineSlug = await createConversationViaAPI(request, "screenshot image", {
      agentTimeout: 90000,
    });
    await page.goto(`/c/${inlineSlug}`);
    const inlineImage = page.locator(".message-agent img").first();
    await expect(inlineImage).toBeVisible({ timeout: 30000 });

    await page.locator(".toc-button").click();
    const inlineEntry = page.locator(".toc-popover .toc-entry-eot").filter({
      hasText: "Verified against the real product",
    });
    const inlineThumbnail = inlineEntry.locator(".toc-entry-thumbnail");
    await expect(inlineThumbnail).toBeVisible();
    expect(await inlineThumbnail.getAttribute("src")).toBe(await inlineImage.getAttribute("src"));
    await page.keyboard.press("Escape");

    const toolSlug = await createConversationViaAPI(request, "screenshot", {
      agentTimeout: 90000,
    });
    await page.goto(`/c/${toolSlug}`);
    const toolImage = page.locator(".screenshot-tool img").first();
    await expect(toolImage).toBeVisible({ timeout: 30000 });
    const toolImageSrc = await toolImage.getAttribute("src");
    await page.locator(".screenshot-tool-header").first().click();
    await expect(toolImage).toBeHidden();

    await page.locator(".toc-button").click();
    const toolEntry = page.locator(".toc-popover .toc-entry-image").first();
    const toolThumbnail = toolEntry.locator(".toc-entry-thumbnail");
    await expect(toolThumbnail).toBeVisible();
    expect(await toolThumbnail.getAttribute("src")).toBe(toolImageSrc);

    await toolEntry.click();
    await expect(page.locator(".toc-popover")).toBeHidden();
    await expect(page).toHaveURL(/#t-[a-zA-Z0-9]+$/);
    await expect(page.locator(".screenshot-tool").first()).toHaveClass(/message-highlight/);
  });
});

test.describe("Message action bar", () => {
  test("uses CSS-only hover labels", async ({ page, request }) => {
    const slug = await createConversationViaAPI(request, "echo action bar");
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const message = page
      .locator('[data-testid="message"]')
      .filter({ hasText: "echo action bar" })
      .first();
    await expect(message).toBeVisible({ timeout: 30000 });
    await message.hover();

    const copy = message.getByRole("button", { name: "Copy" });
    await expect(copy).toBeVisible();
    await expect(copy).toHaveAttribute("data-tooltip", "Copy");
  });
});

test.describe("Context usage popup", () => {
  test("toggles from the usage label and closes on outside click", async ({ page, request }) => {
    test.setTimeout(60000);
    const slug = await createConversationViaAPI(request, "echo context usage");
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const label = page.locator(".context-usage-label");
    await expect(label).toBeVisible({ timeout: 30000 });
    // The label reads "<tokens> · <model name>"; the terse visible text is
    // spelled out for assistive tech.
    await expect(label.locator(".context-usage-label-tokens")).not.toBeEmpty();
    // The denominator is only in the name when the model declares a context
    // window, which the predictable test model does.
    await expect(label).toHaveAccessibleName(/^Context usage: .+ of .+ tokens \([\d.]+%\)$/);
    await expect(label).toHaveAttribute("aria-expanded", "false");

    await label.click();
    const popup = page.locator(".chat-context-popup");
    await expect(popup).toBeVisible();
    await expect(popup).toContainText("tokens used");
    await expect(label).toHaveAttribute("aria-expanded", "true");
    // The panel is teleported out of the button's subtree, so aria-controls is
    // the only thing tying the two together. It must resolve to the dialog.
    const panelId = await label.getAttribute("aria-controls");
    expect(panelId).toBeTruthy();
    await expect(page.locator(`#${panelId}`)).toHaveAttribute("role", "dialog");
    // The cost graph is always on (no feature flag) but its usage entries are
    // walked lazily, only once the label is hovered/focused/clicked. It must
    // actually populate — an empty walk renders "No usage data yet."
    await expect(popup.locator(".token-cost-graph")).toBeVisible();
    await expect(popup.locator(".token-cost-graph-svg")).toBeVisible();

    // Clicking the label again toggles it closed.
    await label.click();
    await expect(popup).toBeHidden();
    await expect(label).toHaveAttribute("aria-expanded", "false");

    // Reopen, then an outside click dismisses. aria-expanded must follow the
    // popover even on the dismissal paths that never reach our click handler.
    await label.click();
    await expect(popup).toBeVisible();
    await page.locator(".messages-container").click({ position: { x: 10, y: 10 } });
    await expect(popup).toBeHidden();
    await expect(label).toHaveAttribute("aria-expanded", "false");

    // Escape dismisses too (new with the PrimeVue Popover port).
    await label.click();
    await expect(popup).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(popup).toBeHidden();
    await expect(label).toHaveAttribute("aria-expanded", "false");
  });

  // The status readout is "<cwd> · <tokens> · <model>" on one line, and it has
  // to survive a narrow viewport by ellipsizing the model name — not by
  // overflowing the bar or clipping the token count. This needs min-width: 0 on
  // every element between the flex container and the model name; a single
  // default `min-width: auto` anywhere in that chain floors the whole subtree at
  // its content width and silently disables the ellipsis, which looks like a
  // truncated model name with no "…".
  test("ellipsizes the model name instead of overflowing when narrow", async ({ page, request }) => {
    test.setTimeout(60000);
    await page.setViewportSize({ width: 360, height: 760 });
    const slug = await createConversationViaAPI(request, "echo narrow readout", {
      cwd: READOUT_CWD,
    });
    await page.goto(`/c/${slug}`);
    const input = page.getByTestId("message-input");
    await expect(input).toBeVisible({ timeout: 30000 });

    // Squeeze the bar the way the tightest real layout does: while the agent
    // works, the working indicator and Stop button share the row.
    await input.fill("bash: sleep 5");
    await page.getByTestId("send-button").click();
    await expect(page.getByTestId("agent-thinking")).toBeVisible({ timeout: 20000 });
    await expect(page.locator(".context-usage-label")).toBeVisible();

    // Measures the visible instance: ChatStatusContent is in the DOM twice (the
    // standalone bar and the mobile controls row) and only one is displayed.
    const overflowOf = async (sel: string) => {
      const el = page.locator(`${sel}:visible`).first();
      await expect(el, `${sel} should be present and visible`).toBeVisible();
      return el.evaluate((e) => e.scrollWidth > e.clientWidth + 1);
    };

    // Nothing in the chain may overflow its box...
    expect(await overflowOf(".status-bar-active"), ".status-bar-active overflows").toBe(false);
    expect(await overflowOf(".status-readout"), ".status-readout overflows").toBe(false);
    // ...and the token count in particular is never clipped: a truncated "1"
    // reads as a different number.
    expect(await overflowOf(".context-usage-label-tokens"), "token count clipped").toBe(false);

    // The model name is the part that gives. The invariant that actually broke
    // is the min-width: 0 chain from the readout down to it — assert that
    // directly, since it holds regardless of how long the fixture model's name
    // happens to be.
    // Every ANCESTOR from the readout down to the name's own box must allow
    // shrinking. The name element itself is exempt: it is the thing being
    // clipped, and its min-content floor is what the ellipsis replaces.
    const chain = await page.evaluate(() => {
      const el = [...document.querySelectorAll(".model-picker-value-name")].find(
        (e) => (e as HTMLElement).offsetParent,
      ) as HTMLElement | undefined;
      if (!el) return null;
      const out: { cls: string; minWidth: string; isSelectLabel: boolean }[] = [];
      for (let n = el.parentElement; n; n = n.parentElement) {
        out.push({
          cls: n.className || n.tagName,
          minWidth: getComputedStyle(n).minWidth,
          isSelectLabel: n.classList.contains("p-select-label"),
        });
        if (n.classList.contains("status-readout")) break;
      }
      return out;
    });
    expect(chain, "model name span not found").not.toBeNull();
    expect(chain!.map((n) => n.cls)).toContain("status-readout");
    for (const n of chain!) {
      // PrimeVue's own .p-select-label is a block, not a flex item, so its
      // min-width: auto is inert — it can't floor anything. Only flex items
      // matter here, and every one of ours sets 0 explicitly.
      if (n.isSelectLabel) continue;
      expect(n.minWidth, `${n.cls} must not floor the shrink chain`).toBe("0px");
    }

    // And the ellipsis is really applied, rather than the name being wrapped,
    // hidden, or cut off flush.
    const model = page.locator(".model-picker-value-name:visible").first();
    const m = await model.evaluate((el) => ({
      over: el.scrollWidth > el.clientWidth + 1,
      ellipsis: getComputedStyle(el).textOverflow,
      full: (el.textContent ?? "").trim(),
    }));
    expect(m.full.length, "measured an empty model name span").toBeGreaterThan(0);
    expect(m.ellipsis).toBe("ellipsis");
    // This viewport is narrow enough that the fixture model's name cannot fit.
    // If a future fixture model has a very short name this is the assertion to
    // revisit (narrow the viewport further) — the ones above are the invariant.
    expect(m.over, `"${m.full}" fits at 360px, so this no longer proves anything`).toBe(true);
  });

  // ContextUsageBar isn't remounted on a conversation switch, but the usage
  // walk feeding the graph is lazy and its gate resets per conversation. An
  // open popup therefore has to re-ask, or it shows "No usage data yet."
  // forever for the conversation navigated to.
  test("keeps the graph populated when the conversation changes while open", async ({
    page,
    request,
  }) => {
    test.setTimeout(60000);
    const first = await createConversationViaAPI(request, "echo context usage one");
    const second = await createConversationViaAPI(request, "echo context usage two");
    await page.goto(`/c/${first}`);
    await page.waitForLoadState("domcontentloaded");

    const label = page.locator(".context-usage-label");
    await expect(label).toBeVisible({ timeout: 30000 });
    await label.click();
    const popup = page.locator(".chat-context-popup");
    await expect(popup.locator(".token-cost-graph-svg")).toBeVisible();

    // Client-side navigation (the pushState + popstate pattern App listens
    // for), so the component tree — and the open popover — survives.
    await page.evaluate((slug) => {
      history.pushState({}, "", `/c/${slug}`);
      window.dispatchEvent(new PopStateEvent("popstate"));
    }, second);
    await expect(
      page.locator('[data-testid="message"]').filter({ hasText: "two" }).first(),
    ).toBeVisible();
    await expect(popup.locator(".token-cost-graph-svg")).toBeVisible();
    await expect(popup).not.toContainText("No usage data yet.");
  });
});

// The status readout's two controls have distinct destinations: the token count
// opens the cost/compaction popup, the model name opens the model picker. They
// are adjacent, visually identical, and each other's most likely misclick, so
// pin that they don't lead to the same place.
test.describe("Status readout controls", () => {
  test("token count opens the cost popup, model name opens the picker", async ({
    page,
    request,
  }) => {
    test.setTimeout(60000);
    await page.setViewportSize({ width: 1280, height: 800 });
    const slug = await createConversationViaAPI(request, "echo readout controls");
    await page.goto(`/c/${slug}`);

    const tokens = page.locator(".context-usage-label");
    const model = page.locator(".model-picker-inline .p-select-label");
    const costPopup = page.locator(".chat-context-popup");
    const pickerPanel = page.locator(".model-picker-panel");
    await expect(tokens).toBeVisible({ timeout: 30000 });
    await expect(model).toBeVisible();

    // Token count -> cost popup, and NOT the picker.
    await tokens.click();
    await expect(costPopup).toBeVisible();
    await expect(costPopup).toContainText("tokens used");
    await expect(pickerPanel).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(costPopup).toBeHidden();

    // Model name -> picker, and NOT the cost popup. The picker carries the
    // model list, the current selection, and the reasoning pills.
    await model.click();
    await expect(pickerPanel).toBeVisible();
    await expect(pickerPanel.locator("[role=option]").first()).toBeVisible();
    await expect(pickerPanel.locator(".model-picker-effort-pills")).toBeVisible();
    await expect(costPopup).toBeHidden();
    await page.keyboard.press("Escape");
    await expect(pickerPanel).toBeHidden();
  });

  // The inline picker's overlay is portaled to <body> precisely so it can be
  // clamped to the viewport: anchored to the trigger (append-to="self") it is
  // positioned by relativePosition(), which aligns left edges and never clamps,
  // and the trigger sits at the right edge of the bar — so on a narrow screen the
  // panel ran off the left (measured x = -93 at 360px). Pin that it stays on
  // screen, on the viewport where there is least room.
  test("picker overlay stays on screen on a narrow viewport", async ({ page, request }) => {
    test.setTimeout(60000);
    await page.setViewportSize({ width: 360, height: 760 });
    const slug = await createConversationViaAPI(request, "echo narrow picker", {
      cwd: READOUT_CWD,
    });
    await page.goto(`/c/${slug}`);
    const trigger = page.locator(".model-picker-inline .p-select-label");
    await expect(trigger).toBeVisible({ timeout: 30000 });
    await trigger.click();

    const panel = page.locator(".model-picker-panel");
    await expect(panel).toBeVisible();
    await expect(panel.locator("[role=option]").first()).toBeVisible();
    const box = await panel.boundingBox();
    const vp = page.viewportSize()!;
    expect(box, "overlay has no box").not.toBeNull();
    expect(box!.x, "overlay runs off the left edge").toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width, "overlay runs off the right edge").toBeLessThanOrEqual(vp.width);
    expect(box!.y, "overlay runs off the top").toBeGreaterThanOrEqual(0);
    expect(box!.y + box!.height, "overlay runs off the bottom").toBeLessThanOrEqual(vp.height);
    // The search field is the reason the overlay is worth opening on mobile.
    await expect(panel.locator("input")).toBeVisible();
  });

  // The picker's reasoning pills have the same problem as its model list: for a
  // conversation that already exists, conversation_options are locked server-side
  // (the send path stops resending them once promoted), so a purely local change
  // is a silent no-op that survives until the page is reloaded and then vanishes.
  // Both have to go through /model.
  test("reasoning pill persists server-side", async ({ page, request }) => {
    test.setTimeout(60000);
    await page.setViewportSize({ width: 1280, height: 800 });
    const slug = await createConversationViaAPI(request, "echo reasoning pill");
    await page.goto(`/c/${slug}`);
    const trigger = page.locator(".model-picker-inline .p-select-label");
    await expect(trigger).toBeVisible({ timeout: 30000 });
    await trigger.click();

    const pills = page.locator(".model-picker-effort-pills [role=radio]");
    await expect(pills.first()).toBeVisible();
    // Any pill that isn't already selected, and isn't the "auto" sentinel
    // (that one means "defer to the model", which has no /model spelling).
    const pill = pills.filter({ hasNotText: "auto" }).and(
      page.locator('[aria-checked="false"]'),
    );
    const chosen = (await pill.first().textContent())?.trim();
    expect(chosen, "no unselected reasoning pill to click").toBeTruthy();
    await pill.first().click();

    // The switch is recorded in the log like a model switch is...
    await expect(page.locator('[data-testid="message-modelchange"]').last()).toContainText(
      chosen!,
    );
    // ...and lands in the conversation's persisted options, not just localStorage.
    await expect(async () => {
      const list = await (await request.get("/api/conversations")).json();
      const conv = (list.conversations || list).find(
        (c: { slug?: string }) => c.slug === slug,
      );
      expect(conv?.conversation_options ?? "").toContain(`"thinking_level":"${chosen}"`);
    }).toPass({ timeout: 10000 });

    // And the pill itself follows, which it can only do via the server echo:
    // nothing is applied locally, so without the conversation-options watch the
    // user's own click would appear to do nothing. Reopen only if the pill click
    // dismissed the panel.
    if (!(await pills.first().isVisible())) await trigger.click();
    await expect(pills.first()).toBeVisible();
    const chosenPill = page.getByRole("radio", { name: chosen!, exact: true });
    await expect(chosenPill, "the chosen reasoning pill should end up selected").toHaveAttribute(
      "aria-checked",
      "true",
    );

    // Re-clicking the pill that is already selected must not fire another
    // /model: the pills are radios and re-emit on such a click, and each command
    // rebuilds the agent loop and appends a marker to the log.
    const markersBefore = await page.locator('[data-testid="message-modelchange"]').count();
    const sent: string[] = [];
    page.on("request", (r) => {
      if (r.url().includes("/chat")) sent.push(r.postData() || "");
    });
    await chosenPill.click();
    await page.waitForTimeout(1000);
    expect(sent, "re-selecting the current level should send nothing").toEqual([]);
    expect(await page.locator('[data-testid="message-modelchange"]').count()).toBe(markersBefore);
  });

  // Switching model rebuilds the conversation's loop, and ApplyModelSettings
  // cancels a running turn to do it. Killing the turn the user is watching
  // because they wanted to read the model name is not acceptable, so the
  // control is disabled (and says why) while the agent works.
  test("model picker is disabled while the agent works", async ({ page, request }) => {
    test.setTimeout(60000);
    await page.setViewportSize({ width: 1280, height: 800 });
    const slug = await createConversationViaAPI(request, "echo busy picker", {
      cwd: READOUT_CWD,
    });
    await page.goto(`/c/${slug}`);
    const input = page.getByTestId("message-input");
    await expect(input).toBeVisible({ timeout: 30000 });

    const model = page.locator(".model-picker-inline .p-select-label");
    await expect(model).toBeVisible();
    await expect(page.locator(".model-picker-inline")).not.toHaveClass(/p-disabled/);

    await input.fill("bash: sleep 8");
    await page.getByTestId("send-button").click();
    await expect(page.getByTestId("agent-thinking")).toBeVisible({ timeout: 20000 });

    await expect(page.locator(".model-picker-inline")).toHaveClass(/p-disabled/);
    // Clicking it must not open the picker.
    await model.click({ force: true });
    await expect(page.locator(".model-picker-panel")).toHaveCount(0);
    // It must not look clickable either, while staying legible as a readout.
    const look = await page.locator(".model-picker-inline").evaluate((el) => ({
      opacity: getComputedStyle(el).opacity,
      cursor: getComputedStyle(el.querySelector(".p-select-label")!).cursor,
    }));
    expect(look.cursor).toBe("default");
    expect(Number(look.opacity)).toBeLessThan(1);
    expect(Number(look.opacity)).toBeGreaterThan(0.4);
    // And it explains itself twice over, because the two audiences reach it
    // differently: a screen reader lands on the combobox, so the reason has to
    // be an ARIA attribute there...
    await expect(model).toHaveAttribute("aria-description", /switch models/);
    // ...while a pointer needs a tooltip, which can only hang off the wrapper (a
    // disabled PrimeVue control has pointer-events: none). Park the pointer
    // elsewhere first: the click above left it on the segment, and hovering where
    // the pointer already sits fires no mouseover. The working branch has no
    // .status-message, so use the cwd segment as the neighbour.
    const elsewhere = page.locator(".status-readout-cwd");
    await elsewhere.hover();
    await page.locator(".status-readout-model").hover();
    await expect(page.locator(".p-tooltip-text")).toContainText("switch models");
    // Move off it so the tooltip doesn't sit over the token count.
    await elsewhere.hover();
    await expect(page.locator(".p-tooltip-text")).toHaveCount(0);
    // The token count stays usable — reading costs never cancels anything.
    await page.locator(".context-usage-label").click();
    await expect(page.locator(".chat-context-popup")).toBeVisible();
  });
});
