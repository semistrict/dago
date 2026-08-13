import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";

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
    await expect(page.getByTestId("tool-call-completed")).toBeVisible();
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
    expect(result.capabilities.unavailable).toContain("host_processes");
    expect(result.capabilities.unavailable).toContain("unrestricted_host_filesystem");
    expect(result.capabilities.unavailable).toContain("git");
    expect(result.gitStatus).toBe(501);
    expect(result.capabilityHeader).toBe("unavailable");
  });

  test("just-bash powers terminal commands and the agent execute tool", async ({ page }) => {
    await page.goto(`${basePath}?model=predictable`);
    const input = page.getByTestId("message-input");

    await input.fill(
      "!mkdir -p /workspace/empty-shell; printf 'from terminal\\n' > /workspace/terminal.txt",
    );
    await page.getByRole("button", { name: "Send message" }).click();
    await expect
      .poll(async () => {
        const response = await page.evaluate(async () =>
          (await fetch("/api/list-directory?path=%2Fworkspace")).json(),
        );
        return response.entries.map((entry: { name: string }) => entry.name);
      })
      .toContain("terminal.txt");
    await expect
      .poll(async () => {
        const response = await page.evaluate(async () =>
          (await fetch("/api/list-directory?path=%2Fworkspace")).json(),
        );
        return response.entries.find(
          (entry: { name: string; is_dir: boolean }) => entry.name === "empty-shell",
        );
      })
      .toEqual({ name: "empty-shell", is_dir: true });

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
    const persistedDirectory = await page.evaluate(async () =>
      (await fetch("/api/list-directory?path=%2Fworkspace")).json(),
    );
    expect(persistedDirectory.entries).toContainEqual({ name: "empty-shell", is_dir: true });
  });

  test("conversation lifecycle, drafts, search, queueing, and generations stay local", async ({
    page,
  }) => {
    await page.goto(`${basePath}?model=predictable`);
    const created = await page.evaluate(async () => {
      const response = await fetch("/api/conversations/new", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ message: "echo: lifecycle needle" }),
      });
      return response.json() as Promise<{ conversation_id: string }>;
    });
    await expect
      .poll(() =>
        page.evaluate(async (id) => {
          const loaded = await (await fetch(`/api/conversation/${id}`)).json();
          return loaded.messages.map((message: { type: string }) => message.type);
        }, created.conversation_id),
      )
      .toEqual(["user", "agent"]);

    const forked = await page.evaluate(async (id) => {
      const response = await fetch(`/api/conversation/${id}/fork`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ sequence_id: 2 }),
      });
      return {
        status: response.status,
        conversation: await response.json(),
      } as { status: number; conversation: { conversation_id: string; slug: string } };
    }, created.conversation_id);
    expect(forked.status).toBe(201);
    expect(forked.conversation.conversation_id).not.toBe(created.conversation_id);
    expect(forked.conversation.slug).toContain("fork");

    const draft = await page.evaluate(async () => {
      const response = await fetch("/api/conversations/draft", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          draft: "first draft",
          model: "browser-predictable",
          cwd: "/workspace",
        }),
      });
      return response.json() as Promise<{ conversation_id: string }>;
    });
    await page.evaluate(async (id) => {
      const response = await fetch(`/api/conversation/${id}/draft`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ draft: "saved draft needle" }),
      });
      if (!response.ok) throw new Error(`draft update failed: ${response.status}`);
    }, draft.conversation_id);
    const search = await page.evaluate(async () => {
      const response = await fetch("/api/conversations/search?q=needle");
      return response.json() as Promise<Array<{ conversation_id: string }>>;
    });
    expect(search.map((item) => item.conversation_id)).toEqual(
      expect.arrayContaining([created.conversation_id, draft.conversation_id]),
    );

    const queued = await page.evaluate(async () => {
      const first = await fetch("/api/conversations/new", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ message: "delay: 500ms" }),
      });
      const { conversation_id: id } = (await first.json()) as { conversation_id: string };
      const second = await fetch(`/api/conversation/${id}/chat`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ message: "echo: queued locally", queue: true }),
      });
      return { id, status: second.status, body: await second.json() };
    });
    expect(queued.status).toBe(202);
    expect(queued.body.status).toBe("queued");
    await expect
      .poll(() =>
        page.evaluate(async (id) => {
          const loaded = await (await fetch(`/api/conversation/${id}`)).json();
          return loaded.messages.length;
        }, queued.id),
      )
      .toBe(4);

    const generation = await page.evaluate(async (id) => {
      const clear = await fetch(`/api/conversation/${id}/new-generation`, { method: "POST" });
      const cleared = await clear.json();
      const send = await fetch(`/api/conversation/${id}/chat`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ message: "echo: new generation" }),
      });
      return { current: cleared.current_generation, status: send.status };
    }, created.conversation_id);
    expect(generation).toEqual({ current: 2, status: 202 });
    await expect
      .poll(() =>
        page.evaluate(async (id) => {
          const loaded = await (await fetch(`/api/conversation/${id}`)).json();
          return loaded.messages.at(-1)?.generation;
        }, created.conversation_id),
      )
      .toBe(2);

    await page.reload();
    const restoredDraft = await page.evaluate(async (id) => {
      const response = await fetch(`/api/conversation/${id}`);
      return response.json() as Promise<{ conversation: { draft: string; is_draft: boolean } }>;
    }, draft.conversation_id);
    expect(restoredDraft.conversation).toMatchObject({
      draft: "saved draft needle",
      is_draft: true,
    });
  });

  test("browser files, uploads, export, and capability-gated controls work end to end", async ({
    page,
  }) => {
    await page.goto(`${basePath}?model=predictable`);
    const files = await page.evaluate(async () => {
      const directory = await fetch("/api/create-directory", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: "/workspace/empty-folder" }),
      });
      const write = await fetch("/api/write-file", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: "/workspace/docs/plan.txt", content: "local plan" }),
      });
      const read = await fetch("/api/read-file?path=%2Fworkspace%2Fdocs%2Fplan.txt");
      return { directory: directory.status, write: write.status, read: await read.json() };
    });
    expect(files).toEqual({ directory: 201, write: 200, read: { content: "local plan" } });

    const uploadInput = page.locator("input.message-input-hidden");
    await uploadInput.setInputFiles({
      name: "context.txt",
      mimeType: "text/plain",
      buffer: Buffer.from("attachment content"),
    });
    await expect(page.locator(".message-attachment-ready")).toContainText("context.txt");
    const uploaded = await page.evaluate(async () => {
      const response = await fetch("/api/list-directory?path=%2Fworkspace%2Fuploads");
      return response.json() as Promise<{ entries: Array<{ name: string }> }>;
    });
    expect(uploaded.entries.some((entry) => entry.name.endsWith("-context.txt"))).toBe(true);

    const input = page.getByTestId("message-input");
    await input.fill("echo: export me");
    await page.getByRole("button", { name: "Send message" }).click();
    await expect(page.getByTestId("agent-thinking")).toBeHidden();
    await page.locator(".chat-overflow-menu-wrapper .btn-icon").click();
    await expect(page.locator(".overflow-menu-item").filter({ hasText: /^Diffs$/ })).toHaveCount(0);
    await expect(page.locator(".overflow-menu-item").filter({ hasText: /Git Graph/ })).toHaveCount(
      0,
    );
    const downloadPromise = page.waitForEvent("download");
    await page
      .locator(".overflow-menu-item")
      .filter({ hasText: /Export/ })
      .click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toMatch(/\.md$/);
    const downloadPath = await download.path();
    if (!downloadPath) throw new Error("browser export did not create a file");
    expect(await readFile(downloadPath, "utf8")).toContain("export me");

    await page.keyboard.press("Control+k");
    await expect(page.locator(".command-palette-item").filter({ hasText: /diff/i })).toHaveCount(0);
    await expect(
      page.locator(".command-palette-item").filter({ hasText: /git graph/i }),
    ).toHaveCount(0);

    await page.reload();
    const restored = await page.evaluate(async () => {
      const response = await fetch("/api/list-directory?path=%2Fworkspace");
      return response.json() as Promise<{ entries: Array<{ name: string; is_dir: boolean }> }>;
    });
    expect(restored.entries).toEqual(
      expect.arrayContaining([
        { name: "docs", is_dir: true },
        { name: "empty-folder", is_dir: true },
        { name: "uploads", is_dir: true },
      ]),
    );
  });
});

test("Chrome directory access indexes lazily and writes every selected path through", async ({
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
        const large = await directory.getFileHandle("large.bin", { create: true });
        const largeWritable = await large.createWritable();
        await largeWritable.write(new Uint8Array(8 * 1024 * 1024 + 1));
        await largeWritable.close();
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
  await expect(setup).toContainText("connected-project · 4 files");

  await setup.getByTestId("browser-openai-key-input").fill("browser-test-key");
  await setup.getByRole("button", { name: "Continue" }).click();
  await expect(setup).toBeHidden();

  const imported = await page.evaluate(async () => {
    const response = await fetch("/api/list-directory?path=%2Fworkspace");
    return response.json() as Promise<{ entries: Array<{ name: string }> }>;
  });
  expect(imported.entries.map((entry) => entry.name)).toEqual([
    ".git",
    "large.bin",
    "node_modules",
    "seed.txt",
  ]);

  const oversizedRead = await page.evaluate(async () => {
    const response = await fetch("/api/read-file?path=%2Fworkspace%2Flarge.bin");
    return { status: response.status, body: await response.text() };
  });
  expect(oversizedRead.status).toBe(404);
  expect(oversizedRead.body).toContain("exceeds 8388608 bytes");

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
    if (!response.ok) {
      throw new Error(`browser shell failed: ${response.status} ${await response.text()}`);
    }
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
  expect(protectedContents).toEqual(["hacked\n", "hacked\n"]);

  const agentWrite = await page.evaluate(async () => {
    const response = await fetch("/api/conversations/new", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: "browser-predictable",
        cwd: "/workspace",
        message:
          'tool: write_file {"file_path":"/workspace/agent-tool.txt","content":"written by agent tool"}',
      }),
    });
    return response.status;
  });
  expect(agentWrite).toBe(202);
  await expect
    .poll(() =>
      page.evaluate(async () => {
        const root = await navigator.storage.getDirectory();
        const directory = await root.getDirectoryHandle("connected-project");
        try {
          const handle = await directory.getFileHandle("agent-tool.txt");
          return (await handle.getFile()).text();
        } catch {
          return "";
        }
      }),
    )
    .toBe("written by agent tool");

  const persistedProjectPaths = await page.evaluate(async () => {
    const database = await new Promise<IDBDatabase>((resolve, reject) => {
      const request = indexedDB.open("shelley-wasm-runtime", 1);
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
    const keys = await new Promise<IDBValidKey[]>((resolve, reject) => {
      const request = database.transaction("files", "readonly").objectStore("files").getAllKeys();
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
    database.close();
    return keys.filter((key) =>
      [
        "/workspace/seed.txt",
        "/workspace/result.txt",
        "/workspace/large.bin",
        "/workspace/agent-tool.txt",
      ].includes(String(key)),
    );
  });
  expect(persistedProjectPaths).toEqual([]);

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
  await expect
    .poll(() =>
      page.evaluate(async () => {
        const response = await fetch("/api/list-directory?path=%2Fworkspace");
        const listed = (await response.json()) as { entries: Array<{ name: string }> };
        return listed.entries.map((entry) => entry.name);
      }),
    )
    .toEqual(expect.arrayContaining(["result.txt", "seed.txt"]));
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
  await expect(page.locator('[role="article"]')).toHaveText([
    "answer through the direct provider",
    "direct browser response",
  ]);
  await expect(page.getByTestId("agent-thinking")).toBeHidden();
  const customModel = await page.evaluate(async (customEndpoint) => {
    const response = await fetch("/api/custom-models", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        display_name: "Browser Custom",
        provider_type: "openai-responses",
        endpoint: customEndpoint,
        api_key: "custom-browser-key",
        model_name: "browser-custom-model",
        max_tokens: 32000,
        reasoning_support: "none",
        image_support: "none",
      }),
    });
    return { status: response.status, model: await response.json() } as {
      status: number;
      model: { model_id: string };
    };
  }, endpoint);
  expect(customModel.status).toBe(201);
  expect(customModel.model.model_id).toBeTruthy();
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
  const restoredModels = await page.evaluate(
    async () => (await fetch("/api/custom-models")).json() as Promise<Array<{ model_id: string }>>,
  );
  expect(restoredModels.map((model) => model.model_id)).toContain(customModel.model.model_id);
  await input.fill("continue after restoring the checkpoint");
  await page.getByRole("button", { name: "Send message" }).click();
  await expect(page.locator('[role="article"]')).toHaveText([
    "answer through the direct provider",
    "direct browser response",
    "continue after restoring the checkpoint",
    "direct browser response",
  ]);
  await expect(page.getByTestId("agent-thinking")).toBeHidden();
});
