import { test, expect } from "@playwright/test";

// The unified model + effort picker (ChatStatusContent -> ModelPicker.vue) is
// built on PrimeVue <Select>. It renders on the new-conversation screen. Here
// we exercise the PrimeVue-specific open/select behavior, the inline
// reasoning-effort pill row, the pinned "Manage models…" footer action, and
// persistence of the chosen model + effort to localStorage.
test.describe("Model picker (PrimeVue)", () => {
  test("opens, lists models, selecting one persists, footer opens manage modal", async ({
    page,
  }) => {
    test.setTimeout(60000);

    await page.goto("/new");
    await page.waitForLoadState("domcontentloaded");

    const picker = page.locator(".model-picker.p-select");
    await expect(picker).toBeVisible({ timeout: 10000 });

    // Open the overlay.
    await picker.click();
    const panel = page.locator(".model-picker-panel");
    await expect(panel).toBeVisible();

    // At least one model is offered and the footer actions are present.
    const options = panel.locator(".p-select-option");
    expect(await options.count()).toBeGreaterThanOrEqual(1);
    const manageBtn = panel.getByRole("button", { name: "Manage models…" });
    await expect(manageBtn).toBeVisible();
    await expect(panel.getByRole("button", { name: "Refresh" })).toBeVisible();

    // In a single-source install, no source sub-labels are rendered.
    await expect(panel.locator(".model-picker-option-source")).toHaveCount(0);

    // Pick the first model -> its label shows in the trigger and the raw model
    // id (not the pretty label) persists to localStorage.
    const firstName = (await options
      .first()
      .locator(".model-picker-option-name")
      .textContent())!.trim();
    await options.first().click();
    await expect(panel).toBeHidden();
    await expect(picker.locator(".model-picker-value-name")).toHaveText(firstName);
    expect(await page.evaluate(() => localStorage.getItem("shelley_selected_model"))).toBe(
      "predictable",
    );

    // The footer action opens the manage-models modal.
    await picker.click();
    await expect(panel).toBeVisible();
    await panel.getByRole("button", { name: "Manage models…" }).click();
    await expect(page.getByRole("dialog")).toBeVisible();
  });

  test("keeps model and directory inline when they fit, then wraps when needed", async ({
    page,
  }) => {
    // Pin the directory so layout doesn't depend on the length of the
    // server's checkout path (which varies across CI agents and wraps the
    // Dir chip onto its own line when long).
    await page.addInitScript(() => localStorage.setItem("shelley_selected_cwd", "/tmp/e2e-dir"));
    await page.setViewportSize({ width: 412, height: 915 });
    await page.goto("/new");

    const fieldTops = () =>
      page.evaluate(() => ({
        model: document.querySelector(".status-field-model")!.getBoundingClientRect().top,
        cwd: document.querySelector(".status-field-cwd")!.getBoundingClientRect().top,
      }));

    let tops = await fieldTops();
    expect(Math.abs(tops.model - tops.cwd)).toBeLessThan(2);

    await page.setViewportSize({ width: 320, height: 700 });
    tops = await fieldTops();
    expect(Math.abs(tops.model - tops.cwd)).toBeGreaterThan(2);
  });

  test("effort pills select a level, persist it, and keep the popover open", async ({ page }) => {
    test.setTimeout(60000);

    await page.goto("/new");
    await page.waitForLoadState("domcontentloaded");

    // Reset persisted level so assertions are deterministic across workers.
    await page.evaluate(() => localStorage.removeItem("shelley.thinkingLevel.v2"));
    await page.reload();
    await page.waitForLoadState("domcontentloaded");

    const picker = page.locator(".model-picker.p-select");
    await expect(picker).toBeVisible({ timeout: 10000 });
    await picker.click();
    const panel = page.locator(".model-picker-panel");
    await expect(panel).toBeVisible();

    // The effort radiogroup offers the real levels (no bare "default" when the
    // model advertises no concrete default — the sentinel is labeled "auto").
    const pills = panel.locator(".model-picker-effort-pill");
    expect(await pills.count()).toBeGreaterThanOrEqual(6);

    // Pick "high" -> persists, popover stays open, trigger shows the suffix.
    await pills.filter({ hasText: /^high$/ }).click();
    await expect(panel).toBeVisible();
    expect(await page.evaluate(() => localStorage.getItem("shelley.thinkingLevel.v2"))).toBe(
      "high",
    );
    await expect(pills.filter({ hasText: /^high$/ })).toHaveAttribute("aria-checked", "true");

    // Close the popover; the trigger reflects the effort.
    await page.keyboard.press("Escape");
    await expect(panel).toBeHidden();
    await expect(picker.locator(".model-picker-value-effort")).toHaveText("· high");
  });

  test("subscription sign-in refreshes the catalog and selects Luna", async ({ page }) => {
    let signInStarted = false;
    const luna = {
      id: "gpt-5.6-luna",
      display_name: "GPT-5.6 Luna",
      source: "OpenAI subscription",
      base_url: "https://chatgpt.com",
      api_type: "responses",
      ready: true,
      is_default: true,
      supports_images: true,
    };

    await page.route("**/api/auth/openai/status", async (route) => {
      await route.fulfill({
        json: signInStarted
          ? { state: "complete", ready: true, model_id: luna.id }
          : { state: "", ready: false, model_id: luna.id },
      });
    });
    await page.route("**/api/auth/openai/start", async (route) => {
      signInStarted = true;
      await route.fulfill({
        status: 202,
        json: { authorization_url: "https://example.test/oauth" },
      });
    });
    await page.route("**/api/models/refresh", async (route) => {
      await route.fulfill({ json: [luna] });
    });
    await page.route("**/api/models", async (route) => {
      if (signInStarted) {
        await route.fulfill({ json: [luna] });
      } else {
        await route.continue();
      }
    });
    await page.addInitScript(() => {
      localStorage.removeItem("shelley_selected_model");
      window.open = () => null;
    });

    await page.goto("/new");
    const picker = page.locator(".model-picker.p-select");
    await expect(picker).toBeVisible();
    await picker.click();
    await page
      .locator(".model-picker-panel")
      .getByRole("button", { name: "Manage models…" })
      .click();

    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("OpenAI subscription", { exact: true })).toBeVisible();
    await dialog.getByRole("button", { name: "Sign in with OpenAI" }).click();

    await expect(dialog).toBeHidden();
    await expect(picker.locator(".model-picker-value-name")).toContainText("Luna");
    expect(await page.evaluate(() => localStorage.getItem("shelley_selected_model"))).toBe(luna.id);
  });
});
