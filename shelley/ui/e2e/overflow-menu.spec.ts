import { test, expect } from "@playwright/test";
import { createConversationViaAPI } from "./helpers";

// The top-right overflow ("kebab") menu is built from PrimeVue components
// (Popover + SelectButton + Select). See components/ChatOverflowMenu.vue. The
// DOM contract (.chat-overflow-menu-wrapper / .btn-icon / .overflow-menu-item)
// is covered by other specs (agents-md-vim, diff-viewer-find); here we
// exercise the PrimeVue-specific controls.
test.describe("Overflow menu (PrimeVue)", () => {
  test("popover opens, theme SelectButton and language Select work", async ({
    page,
    request,
  }) => {
    test.setTimeout(60000);

    const slug = await createConversationViaAPI(request, "Hello");
    await page.goto(`/c/${slug}`);
    await page.waitForLoadState("domcontentloaded");

    // Reset persisted prefs so assertions are deterministic regardless of
    // what an earlier test in the same worker stored.
    await page.evaluate(() => {
      localStorage.setItem("shelley-theme", "system");
    });
    await page.reload();
    await page.waitForLoadState("domcontentloaded");

    // Open the PrimeVue Popover.
    const trigger = page.locator(".chat-overflow-menu-wrapper .btn-icon");
    await expect(trigger).toBeVisible({ timeout: 10000 });
    await trigger.click();

    const popover = page.locator(".chat-overflow-popover");
    await expect(popover).toBeVisible();

    // --- Theme SelectButton: switch to Dark -> <html class="dark"> ---
    const darkBtn = popover
      .locator(".p-selectbutton")
      .nth(0)
      .locator(".p-togglebutton")
      .nth(2);
    await darkBtn.click();
    await expect(page.locator("html")).toHaveClass(/dark/);
    expect(await page.evaluate(() => localStorage.getItem("shelley-theme"))).toBe("dark");

    // Back to Light -> no dark class.
    const lightBtn = popover
      .locator(".p-selectbutton")
      .nth(0)
      .locator(".p-togglebutton")
      .nth(1);
    await lightBtn.click();
    await expect(page.locator("html")).not.toHaveClass(/dark/);

    // --- Language Select: open and pick Japanese ---
    const select = popover.locator(".overflow-language-select");
    await select.click();
    // The overlay renders inside the popover (appendTo="self"), so the popover
    // must stay open while we pick.
    const jpOption = page.locator(".p-select-option").filter({ hasText: /日本語/ });
    await expect(jpOption).toBeVisible();
    await jpOption.click();
    expect(await page.evaluate(() => localStorage.getItem("shelley-locale"))).toBe("ja");
    await expect(popover).toBeVisible();

    // The SelectButton option labels must re-translate live (they're computed,
    // not captured once at setup): the theme "Dark" option's sr-only label
    // should now read in Japanese while the popover is still open.
    await expect(
      popover.locator(".p-togglebutton").filter({ hasText: "ダーク" }),
    ).toBeVisible();

    // Reset locale so we don't leak Japanese UI into sibling tests' assertions.
    await page.evaluate(() => localStorage.setItem("shelley-locale", "en"));
  });
});
