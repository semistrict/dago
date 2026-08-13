import type { FullConfig } from "@playwright/test";
import { spawn, spawnSync, type ChildProcessByStdio } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
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

  const servers = [
    startServer(binary, temporary, "default"),
    startServer(binary, temporary, "manual", ["--manual-review"]),
    startServer(binary, temporary, "yolo", ["--yolo"])
  ];

  try {
    const [baseURL, manualURL, yoloURL] = await Promise.all(servers.map(serverURL));
    process.env.PLAYWRIGHT_TEST_BASE_URL = baseURL;
    process.env.PLAYWRIGHT_MANUAL_URL = manualURL;
    process.env.PLAYWRIGHT_YOLO_URL = yoloURL;
  } catch (error) {
    for (const server of servers) server.kill("SIGTERM");
    await rm(temporary, { force: true, recursive: true });
    throw error;
  }

  return async () => {
    await Promise.all(servers.map(stopServer));
    await rm(temporary, { force: true, recursive: true });
  };
}

function startServer(binary: string, temporary: string, name: string, extraArguments: string[] = []) {
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
      env: { ...process.env, OPENAI_API_KEY: "playwright-placeholder", TERM: "xterm-256color" },
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
