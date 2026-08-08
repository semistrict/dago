import { test, expect } from "@playwright/test";
import { createConversationViaAPI, createConversationViaAPIWithDetails } from "./helpers";

test.describe("Scroll behavior", () => {
  test("auto-pinning does not synchronously read back scrollTop", async ({ page, request }) => {
    await page.addInitScript(() => {
      const state = window as Window & {
        __messagesScrollTopWrites?: number;
        __messagesScrollTopReadsAfterWrite?: number;
      };
      const scrollTop = Object.getOwnPropertyDescriptor(Element.prototype, "scrollTop");
      if (!scrollTop?.get || !scrollTop.set)
        throw new Error("Element.scrollTop accessors not found");
      let wroteMessagesScrollTop = false;
      state.__messagesScrollTopWrites = 0;
      state.__messagesScrollTopReadsAfterWrite = 0;
      Object.defineProperty(Element.prototype, "scrollTop", {
        configurable: scrollTop.configurable,
        enumerable: scrollTop.enumerable,
        get() {
          if (
            wroteMessagesScrollTop &&
            this instanceof HTMLElement &&
            this.classList.contains("messages-container")
          ) {
            state.__messagesScrollTopReadsAfterWrite =
              (state.__messagesScrollTopReadsAfterWrite || 0) + 1;
          }
          return scrollTop.get!.call(this);
        },
        set(value: number) {
          if (this instanceof HTMLElement && this.classList.contains("messages-container")) {
            state.__messagesScrollTopWrites = (state.__messagesScrollTopWrites || 0) + 1;
            wroteMessagesScrollTop = true;
            queueMicrotask(() => {
              wroteMessagesScrollTop = false;
            });
          }
          scrollTop.set!.call(this, value);
        },
      });
    });

    const generated = await request.post("/debug/loremipsum?json=1", {
      form: { size: "medium", model: "predictable" },
    });
    expect(generated.ok()).toBeTruthy();
    const { conversation_id: conversationId } = await generated.json();

    await page.goto(`/c/${conversationId}`);
    await expect(page.locator('[data-testid="message-input"]')).toBeVisible({ timeout: 30000 });
    await expect(page.getByTestId("message").first()).toBeVisible({ timeout: 30000 });
    await expect
      .poll(() =>
        page.evaluate(
          () =>
            (window as Window & { __messagesScrollTopWrites?: number }).__messagesScrollTopWrites ||
            0,
        ),
      )
      .toBeGreaterThan(0);
    expect(
      await page.evaluate(
        () =>
          (window as Window & { __messagesScrollTopReadsAfterWrite?: number })
            .__messagesScrollTopReadsAfterWrite || 0,
      ),
    ).toBe(0);
  });

  test("shows scroll-to-bottom button when scrolled up, auto-scrolls when at bottom", async ({
    page,
    request,
  }) => {
    // Seed a conversation with enough content via the API so we don't race
    // with other tests on the shared server (page.goto('/') used to pick up
    // whichever conversation was most recent, often mid-stream).
    const slug = await createConversationViaAPI(request, "echo message 0");
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const input = page.locator('[data-testid="message-input"]');
    const sendButton = page.locator('[data-testid="send-button"]');
    await expect(input).toBeVisible({ timeout: 30000 });

    // Add more messages to ensure we have scrollable content.
    for (let i = 1; i < 4; i++) {
      await input.fill(`echo message ${i}`);
      await sendButton.click();
      // Wait for the agent reply for this specific message to appear.
      await expect(page.locator(`text=echo message ${i}`).last()).toBeVisible({ timeout: 30000 });
      await expect(page.getByTestId("agent-thinking")).toBeHidden({ timeout: 30000 });
    }

    // Get the messages container
    const messagesContainer = page.locator(".messages-container");
    const scrollButton = page.locator(".scroll-to-bottom-button");

    // The TOC must not synchronously measure every message during scroll.
    // That forces Safari to lay out content-visibility:auto messages that are
    // far off screen and turns one scroll event into a long main-thread stall.
    await page.evaluate(() => {
      const state = window as Window & {
        __messageRectReads?: number;
        __messagesScrollHeightReads?: number;
      };
      const originalRect = Element.prototype.getBoundingClientRect;
      const scrollHeight = Object.getOwnPropertyDescriptor(Element.prototype, "scrollHeight");
      const scrollTop = Object.getOwnPropertyDescriptor(Element.prototype, "scrollTop");
      if (!scrollHeight?.get) throw new Error("Element.scrollHeight getter not found");
      if (!scrollTop?.get || !scrollTop.set)
        throw new Error("Element.scrollTop accessors not found");
      state.__messageRectReads = 0;
      state.__messagesScrollHeightReads = 0;
      Element.prototype.getBoundingClientRect = function () {
        if (this instanceof HTMLElement && this.hasAttribute("data-message-id")) {
          state.__messageRectReads = (state.__messageRectReads || 0) + 1;
        }
        return originalRect.call(this);
      };
      Object.defineProperty(Element.prototype, "scrollHeight", {
        configurable: scrollHeight.configurable,
        enumerable: scrollHeight.enumerable,
        get() {
          if (this instanceof HTMLElement && this.classList.contains("messages-container")) {
            state.__messagesScrollHeightReads = (state.__messagesScrollHeightReads || 0) + 1;
          }
          return scrollHeight.get!.call(this);
        },
      });
      Object.defineProperty(Element.prototype, "scrollTop", {
        configurable: scrollTop.configurable,
        enumerable: scrollTop.enumerable,
        get() {
          return scrollTop.get!.call(this);
        },
        set(value: number) {
          // WebKit can wrap extremely large scroll offsets back to zero.
          scrollTop.set!.call(this, value > 0x7fffffff ? 0 : value);
        },
      });
    });

    // Scroll up to the top and verify the scroll-to-bottom button appears.
    //
    // Setting scrollTop dispatches the 'scroll' event asynchronously, so the
    // component's userScrolled flag isn't set synchronously. Under CI load a
    // late streaming delta can fire the ResizeObserver before that scroll
    // event lands and auto-scroll us back to the bottom, hiding the button for
    // good. Re-scroll inside a poll so such a yank-back can't permanently fail
    // the test, then assert the button stays visible once it's settled.
    await expect(async () => {
      await page.evaluate(() => {
        const state = window as Window & {
          __messageRectReads?: number;
          __messagesScrollHeightReads?: number;
        };
        state.__messageRectReads = 0;
        state.__messagesScrollHeightReads = 0;
      });
      await messagesContainer.evaluate((el) => {
        el.scrollTop = 0;
      });
      await expect(scrollButton).toBeVisible({ timeout: 1000 });
    }).toPass({ timeout: 30000 });
    await expect
      .poll(() =>
        page.evaluate(
          () => (window as Window & { __messageRectReads?: number }).__messageRectReads || 0,
        ),
      )
      .toBe(0);
    await expect
      .poll(() =>
        page.evaluate(
          () =>
            (window as Window & { __messagesScrollHeightReads?: number })
              .__messagesScrollHeightReads || 0,
        ),
      )
      .toBe(0);

    await page.locator(".toc-button").click();
    await expect(page.locator(".toc-entry-top")).toHaveClass(/toc-entry-active/);

    // The TOC's bottom action must stay pinned while lazy content expands.
    await messagesContainer.evaluate((container) => {
      const list = container.querySelector(".messages-list");
      const sentinel = container.querySelector(".messages-bottom-sentinel");
      if (!list || !sentinel) throw new Error("message list sentinel not found");
      const spacer = document.createElement("div");
      spacer.dataset.testid = "lazy-bottom-growth";
      list.insertBefore(spacer, sentinel);
      let height = 0;
      const grow = () => {
        height += 400;
        spacer.style.height = `${height}px`;
        if (height < 1200) requestAnimationFrame(grow);
      };
      requestAnimationFrame(grow);
    });
    await page.locator(".toc-entry-bottom").click();
    await expect
      .poll(() =>
        messagesContainer.evaluate(
          (el) => Math.abs(el.scrollHeight - el.clientHeight - el.scrollTop) <= 1,
        ),
      )
      .toBe(true);

    await expect(async () => {
      await messagesContainer.evaluate((el) => {
        el.scrollTop = 0;
      });
      await expect(scrollButton).toBeVisible({ timeout: 1000 });
    }).toPass({ timeout: 30000 });

    // Click the button to return to the bottom. A late streaming-driven
    // auto-scroll may beat us to it and hide the button first; that's fine —
    // either path leaves us pinned at the bottom, which is what we're after.
    if (await scrollButton.isVisible()) {
      await scrollButton.click().catch(() => {});
    }

    // Button should disappear once we're back at bottom
    await expect(scrollButton).not.toBeVisible({ timeout: 5000 });

    // An upward wheel gesture must immediately release lazy-layout pinning.
    await messagesContainer.evaluate((el) => {
      el.dispatchEvent(new WheelEvent("wheel", { deltaY: -200, bubbles: true }));
      el.scrollTop = 0;
    });
    await expect(scrollButton).toBeVisible({ timeout: 5000 });
    // A large upward jump (for example Home or a scrollbar drag) must also
    // release the pin, even when no wheel event preceded it.
    await messagesContainer.evaluate((container) => {
      const button = document.querySelector<HTMLButtonElement>(".scroll-to-bottom-button");
      const list = container.querySelector(".messages-list");
      const sentinel = container.querySelector(".messages-bottom-sentinel");
      if (!button || !list || !sentinel) throw new Error("scroll controls not found");
      button.click();
      const spacer = document.createElement("div");
      spacer.style.height = "1200px";
      list.insertBefore(spacer, sentinel);
      container.scrollTop = Math.max(0, container.scrollTop - 200);
    });
    await expect
      .poll(() =>
        messagesContainer.evaluate((el) => el.scrollHeight - el.clientHeight - el.scrollTop > 100),
      )
      .toBe(true);
    await expect(scrollButton).toBeVisible({ timeout: 5000 });
    await scrollButton.click();
    await expect
      .poll(() =>
        messagesContainer.evaluate(
          (el) => Math.abs(el.scrollHeight - el.clientHeight - el.scrollTop) <= 1,
        ),
      )
      .toBe(true);

    // Send another message - should auto-scroll since we're at bottom
    await input.fill("echo final message");
    await sendButton.click();

    // Wait for the user message to appear (predictable is fast, so don't
    // race on the transient agent-thinking indicator).
    await expect(page.locator("text=echo final message").last()).toBeVisible({ timeout: 30000 });

    // Button should not appear since we're following the conversation
    await expect(scrollButton).not.toBeVisible({ timeout: 5000 });

    // Regression: after a full reload the conversation renders through the
    // loading spinner, which recreates .messages-list. The scroll observers
    // must re-attach to the new nodes; otherwise the IntersectionObserver
    // stays bound to detached DOM and the button never hides at the bottom
    // (and streaming auto-scroll silently stops). Reload and prove the button
    // toggles correctly against the freshly rendered list.
    await page.reload();
    await page.waitForLoadState("domcontentloaded");
    await expect(input).toBeVisible({ timeout: 30000 });
    await expect(messagesContainer).toBeVisible({ timeout: 30000 });

    await expect(async () => {
      await messagesContainer.evaluate((el) => {
        el.scrollTop = 0;
      });
      await expect(scrollButton).toBeVisible({ timeout: 1000 });
    }).toPass({ timeout: 30000 });

    await messagesContainer.evaluate((el) => {
      el.scrollTop = el.scrollHeight;
    });
    await expect(scrollButton).not.toBeVisible({ timeout: 5000 });
  });

  test("a sub-margin scrollTop clamp does not strand the scroll-to-bottom button", async ({
    page,
    request,
  }) => {
    // Regression for a CI flake (builds 8584/8621/8634): the button latched on
    // while the container was sitting at the bottom, so it never went away.
    //
    // handleScroll treated *any* upward scrollTop delta as "user scrolled up".
    // Drops smaller than the bottom sentinel's 100px rootMargin leave the
    // sentinel intersecting, and an IntersectionObserver only reports changes,
    // so it had already said "at bottom" and would not fire again to correct
    // the button. Such small clamps are routine rather than exotic:
    // content-visibility:auto chunks swap estimated heights for real ones as
    // they lay out, which nudges scrollTop down by a few pixels. On a loaded
    // CI host that reordering is easy to hit; locally it usually isn't, which
    // is what made this look flaky instead of broken.
    const slug = await createConversationViaAPI(request, "echo message 0");
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const input = page.locator('[data-testid="message-input"]');
    const sendButton = page.locator('[data-testid="send-button"]');
    const scrollButton = page.locator(".scroll-to-bottom-button");
    const messagesContainer = page.locator(".messages-container");
    await expect(input).toBeVisible({ timeout: 30000 });

    // Enough content that the list actually scrolls.
    for (let i = 1; i < 4; i++) {
      await input.fill(`echo message ${i}`);
      await sendButton.click();
      await expect(page.locator(`text=echo message ${i}`).last()).toBeVisible({ timeout: 30000 });
      await expect(page.getByTestId("agent-thinking")).toBeHidden({ timeout: 30000 });
    }
    await expect
      .poll(() =>
        messagesContainer.evaluate(
          (el) => Math.abs(el.scrollHeight - el.clientHeight - el.scrollTop) <= 1,
        ),
      )
      .toBe(true);
    await expect(scrollButton).not.toBeVisible({ timeout: 5000 });

    // scrollToBottom's rAF pin re-pins scrollTop every frame for ~120 frames,
    // which would paper over the clamp. Retry the nudge until it survives two
    // frames: that is precisely the point the pin has lapsed, so the scroll
    // event is handled the way a mid-conversation clamp would be. (Polling on
    // the synchronous gap instead would succeed on the very first try, while
    // the pin was still active and suppressing the code path under test.)
    await expect
      .poll(
        () =>
          messagesContainer.evaluate(async (el) => {
            el.scrollTop = el.scrollTop - 5;
            await new Promise((resolve) =>
              requestAnimationFrame(() => requestAnimationFrame(resolve)),
            );
            return el.scrollHeight - el.clientHeight - el.scrollTop;
          }),
        { timeout: 15000 },
      )
      .toBeGreaterThan(0);

    // The scroll event has been delivered by now (it precedes those frames), so
    // this is a settled state, not a race: still following the conversation, so
    // the button must be hidden and auto-follow must still be armed.
    expect(await scrollButton.isVisible()).toBe(false);
    await input.fill("echo after clamp");
    await sendButton.click();
    await expect(page.locator("text=echo after clamp").last()).toBeVisible({ timeout: 30000 });
    await expect(scrollButton).not.toBeVisible({ timeout: 5000 });
  });

  test("restores to the true bottom after reload despite content-visibility height estimates", async ({
    page,
    request,
  }) => {
    // Regression for the "chat jumps toward the top / stops following" bug
    // (GitHub #245). Message rows are wrapped in .messages-chunk elements with
    // content-visibility:auto and a contain-intrinsic-size estimate. Off-screen
    // chunks report the *estimate* rather than their real height, so a reload
    // computes an inflated scrollHeight. Persisting a numeric scrollTop then no
    // longer lands at the bottom: the near-bottom check fails and auto-follow
    // is silently disarmed. Firefox estimates more aggressively, but it
    // reproduces on Chromium too, so we assert it here in the default project.
    //
    // The fix persists a layout-free "at bottom" sentinel (from the bottom
    // sentinel's IntersectionObserver) instead of a raw offset, so an
    // at-bottom conversation re-pins to the real bottom on restore.
    const { conversationId, slug } = await createConversationViaAPIWithDetails(
      request,
      "echo seed message",
    );

    // Build enough turns to span several content-visibility chunks (~50 rows
    // per chunk; each predictable turn adds a user + agent row). Seed via the
    // API, serializing on each turn's completion — posting a new chat while the
    // agent is still working on the previous one gets dropped.
    const TURNS = 32;
    for (let i = 0; i < TURNS; i++) {
      const resp = await request.post(`/api/conversation/${conversationId}/chat`, {
        data: { message: `echo bulk message number ${i}`, model: "predictable" },
      });
      expect(resp.ok(), `chat failed: ${resp.status()}`).toBeTruthy();
      const want = i + 2; // +1 for the seed turn, +1 for this one
      await expect(async () => {
        const r = await request.get(`/api/conversation/${conversationId}`);
        expect(r.ok()).toBeTruthy();
        const body = await r.json();
        const turns = (body.messages || []).filter(
          (m: { type: string; end_of_turn?: boolean }) => m.type === "agent" && m.end_of_turn,
        );
        expect(turns.length).toBeGreaterThanOrEqual(want);
      }).toPass({ timeout: 30000 });
    }

    // Narrow viewport => more wrapped rows => taller content => more chunks.
    await page.setViewportSize({ width: 480, height: 640 });
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const input = page.locator('[data-testid="message-input"]');
    const sendButton = page.locator('[data-testid="send-button"]');
    const messagesContainer = page.locator(".messages-container");
    const scrollButton = page.locator(".scroll-to-bottom-button");
    await expect(input).toBeVisible({ timeout: 30000 });
    await expect(messagesContainer).toBeVisible({ timeout: 30000 });

    // Confirm we actually built multiple content-visibility chunks; otherwise
    // the test isn't exercising the estimate path and would pass vacuously.
    await expect
      .poll(() => page.locator(".messages-chunk").count(), { timeout: 30000 })
      .toBeGreaterThanOrEqual(2);

    // Scroll to the true bottom, then reload. The saved position must restore
    // to the bottom, not somewhere up the (inflated) scrollback. Use the app's
    // own scroll-to-bottom (button click drives a RAF re-pin loop) rather than
    // a one-shot scrollTop assignment, which lands short while off-screen
    // content-visibility chunks still report estimated heights.
    if (await scrollButton.isVisible()) {
      await scrollButton.click();
    }
    await expect(scrollButton).not.toBeVisible({ timeout: 10000 });

    await page.reload();
    await page.waitForLoadState("domcontentloaded");
    await expect(input).toBeVisible({ timeout: 30000 });
    await expect(messagesContainer).toBeVisible({ timeout: 30000 });

    // After restore we must be pinned at the bottom: the button stays hidden
    // and the gap to the bottom is ~0 (allowing a small tolerance for the
    // near-bottom margin).
    await expect(scrollButton).not.toBeVisible({ timeout: 10000 });
    await expect
      .poll(
        () => messagesContainer.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight),
        { timeout: 10000 },
      )
      .toBeLessThan(120);

    // And a new message must still auto-scroll into view (auto-follow armed).
    await input.fill("echo after reload");
    await sendButton.click();
    await expect(page.locator("text=echo after reload").last()).toBeVisible({ timeout: 30000 });
    await expect(scrollButton).not.toBeVisible({ timeout: 5000 });
    await expect
      .poll(
        () => messagesContainer.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight),
        { timeout: 10000 },
      )
      .toBeLessThan(120);
  });

  test("a scroll-up the observer has not yet reported still disarms auto-follow", async ({
    page,
    request,
  }) => {
    // The sub-margin clamp fix above made handleScroll defer to the bottom
    // sentinel, which is right for clamps but must not swallow a real gesture.
    // The IntersectionObserver is async, so a scroll event can arrive while
    // sentinelAtBottom is still stale-true. handleScroll then does nothing, and
    // the observer's own callback (which shows the button) does not arm
    // userScrolled -- so auto-follow stayed on and the next list growth yanked
    // the user back to the bottom.
    //
    // Scroll far enough to leave the 100px margin, then check both halves:
    // the button appears, AND new content does not drag the viewport down.
    const slug = await createConversationViaAPI(request, "echo message 0");
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const input = page.locator('[data-testid="message-input"]');
    const sendButton = page.locator('[data-testid="send-button"]');
    const scrollButton = page.locator(".scroll-to-bottom-button");
    const messagesContainer = page.locator(".messages-container");
    await expect(input).toBeVisible({ timeout: 30000 });

    for (let i = 1; i < 5; i++) {
      await input.fill(`echo message ${i}`);
      await sendButton.click();
      await expect(page.locator(`text=echo message ${i}`).last()).toBeVisible({ timeout: 30000 });
    }
    await expect(scrollButton).not.toBeVisible({ timeout: 5000 });

    // A genuine scroll to the very top, dispatched as a bare scrollTop write so
    // no wheel/touch handler runs -- exactly the path where handleScroll is the
    // only chance to notice before the observer catches up.
    await messagesContainer.evaluate((el) => {
      el.scrollTop = 0;
    });
    await expect(scrollButton).toBeVisible({ timeout: 5000 });

    const before = await messagesContainer.evaluate((el) => el.scrollTop);
    expect(before).toBeLessThan(50);

    // Grow the list. Auto-follow must stay disarmed: the user is reading.
    await input.fill("echo message 5");
    await sendButton.click();
    await expect(page.locator("text=echo message 5").last()).toBeAttached({ timeout: 30000 });

    await expect
      .poll(() => messagesContainer.evaluate((el) => el.scrollTop), { timeout: 3000 })
      .toBeLessThan(50);
    await expect(scrollButton).toBeVisible();
  });

  test("a scroll-up racing list growth in the same frame is not overridden", async ({
    page,
    request,
  }) => {
    // Tighter version of the test above. There, the observer had already fired
    // by the time content grew. Here the scroll and the growth land together, so
    // the ResizeObserver's follow-the-bottom branch runs while both handleScroll
    // (which defers to the observer's flag) and the observer itself are still
    // behind -- and it re-pinned to the bottom, throwing the reader back down.
    const slug = await createConversationViaAPI(request, "echo message 0");
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    const input = page.locator('[data-testid="message-input"]');
    const sendButton = page.locator('[data-testid="send-button"]');
    const scrollButton = page.locator(".scroll-to-bottom-button");
    const messagesContainer = page.locator(".messages-container");
    await expect(input).toBeVisible({ timeout: 30000 });

    for (let i = 1; i < 5; i++) {
      await input.fill(`echo message ${i}`);
      await sendButton.click();
      await expect(page.locator(`text=echo message ${i}`).last()).toBeVisible({ timeout: 30000 });
    }
    await expect(scrollButton).not.toBeVisible({ timeout: 5000 });

    // Scroll to the top and grow the list in the same task, so no observer
    // callback can run in between. Deliberately no wheel event: those handlers
    // only arm auto-follow-off while the bottom pin is active, so relying on
    // one here would hide the bug (with a wheel event this passed even while a
    // bare scroll was yanked from 0 to 1607).
    await messagesContainer.evaluate((el) => {
      el.scrollTop = 0;
      const list = el.querySelector(".messages-list");
      if (list) {
        const filler = document.createElement("div");
        filler.style.height = "800px";
        filler.setAttribute("data-test-filler", "1");
        list.appendChild(filler);
      }
    });

    // The reader must stay where they scrolled to.
    await expect
      .poll(() => messagesContainer.evaluate((el) => el.scrollTop), { timeout: 3000 })
      .toBeLessThan(50);
    await expect(scrollButton).toBeVisible({ timeout: 5000 });
  });
});
