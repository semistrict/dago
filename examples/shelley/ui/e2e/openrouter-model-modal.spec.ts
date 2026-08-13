import { test, expect } from "@playwright/test";

test("adds an OpenRouter model from Manage Models", async ({ page, request }) => {
  const displayName = "DeepSeek V4 Flash 0731 via OpenRouter";
  const modelName = "deepseek/deepseek-v4-flash-0731";
  const endpoint = "https://openrouter.ai/api/v1";
  let testRequest: Record<string, unknown> | null = null;
  let createdModelID = "";

  await page.route("**/api/custom-models-test", async (route) => {
    testRequest = route.request().postDataJSON() as Record<string, unknown>;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ success: true, message: "Test successful" }),
    });
  });

  try {
    await page.goto("/new");
    const picker = page.locator(".model-picker.p-select");
    await expect(picker).toBeVisible();
    await picker.click();
    const panel = page.locator(".model-picker-panel");
    await panel.getByRole("button", { name: "Manage models…" }).click();

    const manageDialog = page.getByRole("dialog", { name: "Manage Models" });
    await expect(manageDialog).toBeVisible();
    await manageDialog.getByRole("button", { name: /Add Model/ }).click();

    const formDialog = page.getByRole("dialog", { name: "Add Model" });
    await expect(formDialog).toBeVisible();
    await formDialog.getByRole("button", { name: "OpenRouter (Responses API)" }).click();
    await expect(formDialog.locator(".endpoint-display")).toHaveText(endpoint);

    await formDialog.getByTestId("model-name-input").fill(modelName);
    await expect(formDialog.getByTestId("model-display-name-input")).toHaveValue(
      "DeepSeek V4 Flash 0731",
    );
    await formDialog.getByTestId("model-display-name-input").fill(displayName);
    await formDialog.getByTestId("model-api-key-input").fill("playwright-openrouter-key");

    await formDialog.getByRole("button", { name: "Test" }).click();
    await expect(formDialog.locator(".test-result.success")).toContainText("Test successful");
    expect(testRequest).toMatchObject({
      provider_type: "openrouter-responses",
      endpoint,
      api_key: "playwright-openrouter-key",
      model_name: modelName,
    });

    await formDialog.getByRole("button", { name: "Add Model" }).click();
    await expect(formDialog).toBeHidden();
    const row = manageDialog.locator("tr").filter({ hasText: displayName });
    await expect(row).toContainText(modelName);
    await expect(row).toContainText("OpenRouter (Responses API)");

    const modelsResponse = await request.get("/api/custom-models");
    expect(modelsResponse.ok()).toBeTruthy();
    const customModels = (await modelsResponse.json()) as Array<{
      model_id: string;
      display_name: string;
      provider_type: string;
    }>;
    const created = customModels.find((model) => model.display_name === displayName);
    expect(created).toBeTruthy();
    expect(created?.provider_type).toBe("openrouter-responses");
    createdModelID = created!.model_id;
  } finally {
    if (createdModelID) {
      await request.delete(`/api/custom-models/${createdModelID}`);
    }
  }
});
