import { expect, test } from "@playwright/test";

const basePath = process.env.SHELLEY_WASM_TEST_BASE_PATH || "/";

test.describe("local browser model", () => {
  test("browser runtime persists agent turns and virtual files across reloads", async ({
    page,
  }) => {
    await page.goto(`${basePath}?model=local`);
    const input = page.getByTestId("message-input");
    await expect(input).toBeVisible();

    await input.fill("hello");
    await page.getByRole("button", { name: "Send message" }).click();
    await expect(page.locator('[role="article"]')).toHaveText(["hello", "Well, hi there!"]);
    await expect(page.getByTestId("agent-thinking")).toBeHidden();
    await expect(page).toHaveURL(/\/c\/hello$/);

    await page.reload();
    await expect(page.locator('[role="article"]')).toHaveText(["hello", "Well, hi there!"]);

    await input.fill("echo: after reload");
    await page.getByRole("button", { name: "Send message" }).click();
    await expect(page.locator('[role="article"]')).toHaveText([
      "hello",
      "Well, hi there!",
      "echo: after reload",
      "after reload",
    ]);
    await expect(page.getByTestId("agent-thinking")).toBeHidden();

    await input.fill('tool: write_file {"file_path":"/workspace/note.txt","content":"durable"}');
    await page.getByRole("button", { name: "Send message" }).click();
    await expect(page.getByTestId("agent-thinking")).toBeHidden();

    await page.reload();
    const directory = await page.evaluate(async () => {
      const response = await fetch("/api/list-directory?path=%2Fworkspace");
      return response.json() as Promise<{ entries: Array<{ name: string }> }>;
    });
    expect(directory.entries.map((entry) => entry.name)).toEqual(["README.md", "note.txt"]);
    await expect(page.locator('[role="article"]')).toHaveCount(7);
  });

  test("browser runtime reports host-only capabilities as unavailable", async ({ page }) => {
    await page.goto(`${basePath}?model=local`);
    const result = await page.evaluate(async () => {
      const capabilities = await (await fetch("/api/capabilities")).json();
      const response = await fetch("/api/git/diffs?cwd=%2Fworkspace");
      return {
        capabilities,
        gitStatus: response.status,
        capabilityHeader: response.headers.get("X-Shelley-Capability"),
      };
    });
    expect(result.capabilities.runtime).toBe("wasm");
    expect(result.capabilities.local).toContain("shell");
    expect(result.capabilities.unavailable).not.toContain("shell");
    expect(result.capabilities.unavailable).toContain("pty");
    expect(result.capabilities.unavailable).toContain("host_filesystem");
    expect(result.gitStatus).toBe(501);
    expect(result.capabilityHeader).toBe("unavailable");
  });

  test("just-bash powers terminal commands and the agent execute tool", async ({ page }) => {
    await page.goto(`${basePath}?model=local`);
    const input = page.getByTestId("message-input");

    await input.fill("!printf 'from terminal\\n' > /workspace/terminal.txt");
    await page.getByRole("button", { name: "Send message" }).click();
    await expect
      .poll(async () => {
        const response = await page.evaluate(async () =>
          (await fetch("/api/list-directory?path=%2Fworkspace")).json(),
        );
        return response.entries.map((entry: { name: string }) => entry.name);
      })
      .toContain("terminal.txt");

    const terminalRead = await page.evaluate(async () => {
      const response = await fetch("/api/browser-shell", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ command: "cat /workspace/terminal.txt" }),
      });
      return response.json() as Promise<{ output: string; exit_code: number }>;
    });
    expect(terminalRead).toMatchObject({ output: "from terminal\n", exit_code: 0 });

    await input.fill('tool: execute {"command":"printf \'from agent\\n\' > /workspace/agent.txt"}');
    await page.getByRole("button", { name: "Send message" }).click();
    await expect
      .poll(async () => {
        const response = await page.evaluate(async () =>
          (await fetch("/api/list-directory?path=%2Fworkspace")).json(),
        );
        return response.entries.map((entry: { name: string }) => entry.name);
      })
      .toContain("agent.txt");

    await page.reload();
    const persisted = await page.evaluate(async () =>
      (
        await fetch("/api/browser-shell", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ command: "cat /workspace/agent.txt" }),
        })
      ).json(),
    );
    expect(persisted).toMatchObject({ output: "from agent\n", exit_code: 0 });
  });
});

test("browser saves the OpenAI key locally and restores it after reload", async ({ page }) => {
  const endpoint = process.env.SHELLEY_OPENAI_MOCK_URL;
  if (!endpoint) throw new Error("SHELLEY_OPENAI_MOCK_URL is not configured");
  await page.addInitScript((testEndpoint) => {
    sessionStorage.setItem("shelley_wasm_openai_test_endpoint", testEndpoint);
  }, endpoint);
  await page.goto(basePath);
  const keyDialog = page.getByRole("dialog", { name: "Connect OpenAI" });
  await expect(keyDialog).toBeVisible();
  await expect(keyDialog.locator("input")).toHaveCount(1);
  await expect(keyDialog).not.toContainText("Endpoint");
  await expect(keyDialog).not.toContainText("Model name");
  await expect(keyDialog).not.toContainText("Reasoning");

  const layout = await keyDialog.evaluate((dialog) => {
    const panel = dialog.querySelector<HTMLElement>(".browser-key-modal");
    const body = dialog.querySelector<HTMLElement>(".modal-body");
    const input = dialog.querySelector<HTMLElement>("input");
    const button = dialog.querySelector<HTMLElement>("button");
    const title = dialog.querySelector<HTMLElement>(".modal-title");
    if (!panel || !body || !input || !button || !title)
      throw new Error("OpenAI dialog is incomplete");
    const panelBox = panel.getBoundingClientRect();
    const inputBox = input.getBoundingClientRect();
    const buttonBox = button.getBoundingClientRect();
    const titleStyle = getComputedStyle(title);
    return {
      bodyScrollLeft: body.scrollLeft,
      bodyScrollWidth: body.scrollWidth,
      bodyClientWidth: body.clientWidth,
      inputInside: inputBox.left >= panelBox.left && inputBox.right <= panelBox.right,
      buttonInside: buttonBox.left >= panelBox.left && buttonBox.right <= panelBox.right,
      titleMarginTop: titleStyle.marginTop,
      titleMarginBottom: titleStyle.marginBottom,
    };
  });
  expect(layout).toEqual({
    bodyScrollLeft: 0,
    bodyScrollWidth: layout.bodyClientWidth,
    bodyClientWidth: layout.bodyClientWidth,
    inputInside: true,
    buttonInside: true,
    titleMarginTop: "0px",
    titleMarginBottom: "0px",
  });

  await keyDialog.getByTestId("browser-openai-key-input").fill("browser-test-key");
  await keyDialog.getByRole("button", { name: "Continue" }).click();
  await expect(keyDialog).toBeHidden();
  await expect(page).toHaveURL(/\/new$/);

  const picker = page.locator(".model-picker.p-select");
  await expect(picker).toContainText("GPT-5.6 Luna");
  await picker.click();
  const panel = page.locator(".model-picker-panel");
  await expect(panel).toContainText("GPT-5.6 Luna");
  await expect(panel).toContainText("GPT-5.6 Terra");
  await expect(panel).toContainText("GPT-5.6 Sol");
  await page.keyboard.press("Escape");
  const input = page.locator("textarea");
  await input.fill("answer through the direct provider");
  await page.getByRole("button", { name: "Send message" }).click();
  await expect(page.getByTestId("agent-thinking")).toBeHidden();
  await expect(page.locator('[role="article"]')).toHaveText([
    "answer through the direct provider",
    "direct browser response",
  ]);
  const persisted = await page.evaluate(async () => {
    const durable = await new Promise<string>((resolve, reject) => {
      const open = indexedDB.open("shelley-wasm", 1);
      open.onerror = () => reject(open.error);
      open.onsuccess = () => {
        const database = open.result;
        const request = database
          .transaction("state", "readonly")
          .objectStore("state")
          .get("application-state");
        request.onerror = () => reject(request.error);
        request.onsuccess = () => {
          database.close();
          resolve(typeof request.result === "string" ? request.result : "");
        };
      };
    });
    return {
      local: localStorage.getItem("shelley_wasm_openai_key"),
      session: sessionStorage.getItem("shelley_wasm_openai_key"),
      durable,
    };
  });
  expect(persisted.local).toBe("browser-test-key");
  expect(persisted.session).toBeNull();
  expect(persisted.durable).not.toContain("browser-test-key");

  await page.reload();
  await expect(keyDialog).toBeHidden();
  await expect(page.locator(".model-picker.p-select")).toContainText("GPT-5.6 Luna");
});
