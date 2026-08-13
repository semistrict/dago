import type { FullConfig } from "@playwright/test";
import { spawn, spawnSync, type ChildProcessByStdio } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../..");

export default async function globalSetup(_config: FullConfig): Promise<() => Promise<void>> {
  const temporary = await mkdtemp(path.join(tmpdir(), "dacode-xterm-e2e-"));
  const binary = path.join(temporary, "dacode");
  const build = spawnSync("go", ["build", "-o", binary, "./cmd/dacode"], {
    cwd: root,
    encoding: "utf8"
  });
  if (build.status !== 0) {
    await rm(temporary, { force: true, recursive: true });
    throw new Error(`failed to build dacode:\n${build.stderr}`);
  }

  const slowAPI = createServer((request, response) => {
    request.resume();
    setTimeout(() => {
      response.writeHead(200, { "content-type": "application/json" });
      response.end(
        JSON.stringify({
          id: "response-playwright",
          status: "completed",
          output: [{ type: "message", id: "message-playwright", role: "assistant", content: [] }],
          usage: { input_tokens: 1, output_tokens: 0, total_tokens: 1 }
        })
      );
    }, 2_000);
  });
  slowAPI.listen(0, "127.0.0.1");
  await once(slowAPI, "listening");
  const slowAddress = slowAPI.address();
  if (!slowAddress || typeof slowAddress === "string") {
    slowAPI.close();
    await rm(temporary, { force: true, recursive: true });
    throw new Error("failed to start slow Responses API fixture");
  }
  const slowAPIURL = `http://127.0.0.1:${slowAddress.port}`;

  const servers = [
    startServer(binary, temporary, "default", slowAPIURL),
    startServer(binary, temporary, "manual", slowAPIURL, ["--manual-review"]),
    startServer(binary, temporary, "yolo", slowAPIURL, ["--yolo"])
  ];

  try {
    const [baseURL, manualURL, yoloURL] = await Promise.all(servers.map(serverURL));
    process.env.PLAYWRIGHT_TEST_BASE_URL = baseURL;
    process.env.PLAYWRIGHT_MANUAL_URL = manualURL;
    process.env.PLAYWRIGHT_YOLO_URL = yoloURL;
  } catch (error) {
    for (const server of servers) server.kill("SIGTERM");
    slowAPI.closeAllConnections();
    slowAPI.close();
    await rm(temporary, { force: true, recursive: true });
    throw error;
  }

  return async () => {
    await Promise.all(servers.map(stopServer));
    slowAPI.closeAllConnections();
    await new Promise<void>((resolve) => slowAPI.close(() => resolve()));
    await rm(temporary, { force: true, recursive: true });
  };
}

function startServer(
  binary: string,
  temporary: string,
  name: string,
  baseURL: string,
  extraArguments: string[] = []
) {
  return spawn(
    binary,
    [
      "--serve-xtermjs",
      "--xtermjs-address",
      "127.0.0.1:0",
      "--state-dir",
      path.join(temporary, `state-${name}`),
      "--cwd",
      root,
      ...extraArguments
    ],
    {
      cwd: root,
      env: {
        ...process.env,
        OPENAI_API_KEY: "playwright-placeholder",
        OPENAI_BASE_URL: baseURL,
        TERM: "xterm-256color"
      },
      stdio: ["ignore", "pipe", "pipe"]
    }
  );
}

async function stopServer(server: ChildProcessByStdio<null, Readable, Readable>): Promise<void> {
  if (server.exitCode !== null) return;
  server.kill("SIGTERM");
  await Promise.race([
    new Promise<void>((resolve) => server.once("exit", () => resolve())),
    new Promise<void>((resolve) => setTimeout(resolve, 5_000))
  ]);
}

function serverURL(server: ChildProcessByStdio<null, Readable, Readable>): Promise<string> {
  return new Promise((resolve, reject) => {
    let stdout = "";
    let stderr = "";
    const timeout = setTimeout(() => reject(new Error(`timed out waiting for xterm.js server:\n${stderr}`)), 30_000);
    server.stdout.setEncoding("utf8");
    server.stderr.setEncoding("utf8");
    server.stdout.on("data", (chunk: string) => {
      stdout += chunk;
      const match = stdout.match(/dacode xterm\.js: (http:\/\/[^\s]+)/);
      if (match) {
        clearTimeout(timeout);
        resolve(match[1]);
      }
    });
    server.stderr.on("data", (chunk: string) => {
      stderr += chunk;
    });
    server.once("exit", (code) => {
      clearTimeout(timeout);
      reject(new Error(`xterm.js server exited with ${code}:\n${stderr}`));
    });
  });
}
