import { expect, test } from "@playwright/test";

const basePath = process.env.SHELLEY_WASM_TEST_BASE_PATH || "/";

test.describe("local browser model", () => {
  test("browser runtime persists agent turns and virtual files across reloads", async ({
    page,
  }) => {
    await page.goto(`${basePath}?model=predictable`);
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
    await page.goto(`${basePath}?model=predictable`);
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
    await page.goto(`${basePath}?model=predictable`);
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

test("Chrome directory access imports and writes through the browser workspace", async ({
  page,
}) => {
  const endpoint = process.env.SHELLEY_OPENAI_MOCK_URL;
  if (!endpoint) throw new Error("SHELLEY_OPENAI_MOCK_URL is not configured");
  await page.addInitScript((testEndpoint) => {
    sessionStorage.setItem("shelley_wasm_openai_test_endpoint", testEndpoint);
    Object.defineProperty(window, "showDirectoryPicker", {
      configurable: true,
      value: async () => {
        const root = await navigator.storage.getDirectory();
        try {
          await root.removeEntry("connected-project", { recursive: true });
        } catch {
          // The test directory does not exist on the first run.
        }
        const directory = await root.getDirectoryHandle("connected-project", { create: true });
        const seed = await directory.getFileHandle("seed.txt", { create: true });
        const writable = await seed.createWritable();
        await writable.write("from local folder\n");
        await writable.close();
        for (const [path, content] of [
          [[".git"], "local git metadata\n"],
          [["node_modules", "pkg"], "local dependency\n"],
        ] as const) {
          let parent = directory;
          for (const part of path) {
            parent = await parent.getDirectoryHandle(part, { create: true });
          }
          const protectedFile = await parent.getFileHandle("protected.txt", { create: true });
          const protectedWritable = await protectedFile.createWritable();
          await protectedWritable.write(content);
          await protectedWritable.close();
        }
        return directory;
      },
    });
  }, endpoint);

  await page.goto(basePath);
  const setup = page.getByRole("dialog", { name: "Set up browser workspace" });
  await setup.getByRole("button", { name: "Open folder" }).click();
  await expect(setup).toContainText("connected-project · 1 files · 2 excluded");

  await setup.getByTestId("browser-openai-key-input").fill("browser-test-key");
  await setup.getByRole("button", { name: "Continue" }).click();
  await expect(setup).toBeHidden();

  const imported = await page.evaluate(async () => {
    const response = await fetch("/api/list-directory?path=%2Fworkspace");
    return response.json() as Promise<{ entries: Array<{ name: string }> }>;
  });
  expect(imported.entries.map((entry) => entry.name)).toEqual(["seed.txt"]);

  await page.evaluate(async () => {
    const response = await fetch("/api/browser-shell", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        command:
          "mkdir -p /workspace/.git /workspace/node_modules/pkg; " +
          "printf 'hacked\\n' > /workspace/.git/protected.txt; " +
          "printf 'hacked\\n' > /workspace/node_modules/pkg/protected.txt; " +
          "printf 'written through wasm\\n' > /workspace/result.txt",
      }),
    });
    if (!response.ok) throw new Error(`browser shell failed: ${response.status}`);
  });
  await expect
    .poll(() =>
      page.evaluate(async () => {
        const root = await navigator.storage.getDirectory();
        const directory = await root.getDirectoryHandle("connected-project");
        const handle = await directory.getFileHandle("result.txt");
        return (await handle.getFile()).text();
      }),
    )
    .toBe("written through wasm\n");
  const protectedContents = await page.evaluate(async () => {
    const root = await navigator.storage.getDirectory();
    const directory = await root.getDirectoryHandle("connected-project");
    const read = async (parts: string[]) => {
      let parent = directory;
      for (const part of parts.slice(0, -1)) parent = await parent.getDirectoryHandle(part);
      const file = await parent.getFileHandle(parts.at(-1) || "");
      return (await file.getFile()).text();
    };
    return Promise.all([
      read([".git", "protected.txt"]),
      read(["node_modules", "pkg", "protected.txt"]),
    ]);
  });
  expect(protectedContents).toEqual(["local git metadata\n", "local dependency\n"]);

  await page.evaluate(async () => {
    const root = await navigator.storage.getDirectory();
    const directory = await root.getDirectoryHandle("connected-project");
    const seed = await directory.getFileHandle("seed.txt");
    const writable = await seed.createWritable();
    await writable.write("changed outside the app\n");
    await writable.close();
  });

  await page.reload();
  await expect(setup).toBeHidden();
  const restored = await page.evaluate(async () => {
    const response = await fetch("/api/list-directory?path=%2Fworkspace");
    return response.json() as Promise<{ entries: Array<{ name: string }> }>;
  });
  expect(restored.entries.map((entry) => entry.name)).toEqual(
    expect.arrayContaining(["result.txt", "seed.txt"]),
  );
  const refreshed = await page.evaluate(async () => {
    const response = await fetch("/api/browser-shell", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ command: "cat /workspace/seed.txt" }),
    });
    return response.json() as Promise<{ output: string }>;
  });
  expect(refreshed.output).toBe("changed outside the app\n");
});

test("browser saves the OpenAI key locally and restores it after reload", async ({ page }) => {
  const endpoint = process.env.SHELLEY_OPENAI_MOCK_URL;
  if (!endpoint) throw new Error("SHELLEY_OPENAI_MOCK_URL is not configured");
  await page.addInitScript((testEndpoint) => {
    sessionStorage.setItem("shelley_wasm_openai_test_endpoint", testEndpoint);
  }, endpoint);
  await page.goto(basePath);
  const keyDialog = page.getByRole("dialog", { name: "Set up browser workspace" });
  await expect(keyDialog).toBeVisible();
  await expect(keyDialog.getByRole("button", { name: "Open folder" })).toBeVisible();
  await expect(keyDialog.getByRole("button", { name: "Use local model" })).toBeVisible();
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
    const durable = await new Promise<{
      stores: string[];
      conversations: unknown[];
      files: unknown[];
      checkpoints: unknown[];
      writes: unknown[];
    }>((resolve, reject) => {
      const open = indexedDB.open("shelley-wasm-runtime", 1);
      open.onerror = () => reject(open.error);
      open.onsuccess = () => {
        const database = open.result;
        const transaction = database.transaction(
          ["conversations", "files", "checkpoints", "checkpoint_writes"],
          "readonly",
        );
        const requests = {
          conversations: transaction.objectStore("conversations").getAll(),
          files: transaction.objectStore("files").getAll(),
          checkpoints: transaction.objectStore("checkpoints").getAll(),
          writes: transaction.objectStore("checkpoint_writes").getAll(),
        };
        transaction.onerror = () => reject(transaction.error);
        transaction.oncomplete = () => {
          database.close();
          resolve({
            stores: [...database.objectStoreNames],
            conversations: requests.conversations.result,
            files: requests.files.result,
            checkpoints: requests.checkpoints.result,
            writes: requests.writes.result,
          });
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
  expect(persisted.durable.stores).not.toContain("state");
  expect(persisted.durable.conversations).toHaveLength(1);
  expect(persisted.durable.checkpoints.length).toBeGreaterThan(0);
  expect(JSON.stringify(persisted.durable)).not.toContain("browser-test-key");

  await page.reload();
  await expect(keyDialog).toBeHidden();
  await expect(page.locator(".model-picker.p-select")).toContainText("GPT-5.6 Luna");
  await input.fill("continue after restoring the checkpoint");
  await page.getByRole("button", { name: "Send message" }).click();
  await expect(page.getByTestId("agent-thinking")).toBeHidden();
  await expect(page.locator('[role="article"]')).toHaveText([
    "answer through the direct provider",
    "direct browser response",
    "continue after restoring the checkpoint",
    "direct browser response",
  ]);
});
