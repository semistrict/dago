import { execSync, spawn, type ChildProcess } from "child_process";
import { createServer, type Server } from "http";
import path from "path";
import { fileURLToPath } from "url";

const filename = fileURLToPath(import.meta.url);
const directory = path.dirname(filename);
const shelleyDirectory = path.resolve(directory, "../..");
const publicBasePath = normalizeBasePath(process.env.PUBLIC_BASE_PATH || "/");
let server: ChildProcess | null = null;
let openAIServer: Server | null = null;

export default async function globalSetup() {
  openAIServer = createServer((request, response) => {
    const allowHeaders =
      request.headers["access-control-request-headers"] || "Authorization, Content-Type";
    response.setHeader("Access-Control-Allow-Origin", "*");
    response.setHeader("Access-Control-Allow-Headers", allowHeaders);
    response.setHeader("Access-Control-Allow-Methods", "POST, OPTIONS");
    if (request.method === "OPTIONS") {
      response.writeHead(204).end();
      return;
    }
    if (request.method !== "POST" || request.url !== "/v1/responses") {
      response.writeHead(404).end();
      return;
    }
    if (request.headers.authorization !== "Bearer browser-test-key") {
      response.writeHead(401, { "Content-Type": "application/json" });
      response.end(JSON.stringify({ error: { message: "unexpected browser authorization" } }));
      return;
    }
    response.writeHead(200, { "Content-Type": "application/json" });
    response.end(
      JSON.stringify({
        id: `resp_browser_${Date.now()}`,
        status: "completed",
        output: [
          {
            type: "message",
            id: `msg_browser_${Date.now()}`,
            role: "assistant",
            content: [{ type: "output_text", text: "direct browser response" }],
          },
        ],
        usage: { input_tokens: 5, output_tokens: 3, total_tokens: 8 },
      }),
    );
  });
  await listen(openAIServer);
  const openAIAddress = openAIServer.address();
  if (!openAIAddress || typeof openAIAddress === "string")
    throw new Error("mock OpenAI address unavailable");
  process.env.SHELLEY_OPENAI_MOCK_URL = `http://127.0.0.1:${openAIAddress.port}/v1`;

  if (process.env.TEST_SERVER_URL) {
    process.env.PLAYWRIGHT_TEST_BASE_URL = process.env.TEST_SERVER_URL;
    return closeServers;
  }

  execSync("make wasm", {
    cwd: shelleyDirectory,
    stdio: "inherit",
    env: { ...process.env, PUBLIC_BASE_PATH: publicBasePath },
  });
  server = spawn(
    "go",
    ["run", "./cmd/shelley-wasm-serve", "-listen", "127.0.0.1:0", "-base-path", publicBasePath],
    {
      cwd: shelleyDirectory,
      stdio: ["ignore", "pipe", "inherit"],
    },
  );
  const baseURL = await serverURL(server);
  process.env.PLAYWRIGHT_TEST_BASE_URL = baseURL;
  process.env.SHELLEY_WASM_TEST_BASE_PATH = publicBasePath;

  return closeServers;
}

async function closeServers() {
  server?.kill("SIGTERM");
  server = null;
  if (openAIServer) await new Promise<void>((resolve) => openAIServer?.close(() => resolve()));
  openAIServer = null;
}

function listen(target: Server): Promise<void> {
  return new Promise((resolve, reject) => {
    target.once("error", reject);
    target.listen(0, "127.0.0.1", () => {
      target.off("error", reject);
      resolve();
    });
  });
}

function serverURL(process: ChildProcess): Promise<string> {
  return new Promise((resolve, reject) => {
    let output = "";
    const onExit = (code: number | null) =>
      reject(new Error(`WASM test server exited with code ${code}`));
    process.once("exit", onExit);
    process.stdout?.on("data", (chunk: Buffer) => {
      output += chunk.toString("utf8");
      const match = output.match(/Starting browser-native Shelley at (http:\/\/[^/]+)/);
      if (!match) return;
      process.off("exit", onExit);
      resolve(match[1]);
    });
  });
}

function normalizeBasePath(value: string): string {
  const withLeadingSlash = value.startsWith("/") ? value : `/${value}`;
  return withLeadingSlash.endsWith("/") ? withLeadingSlash : `${withLeadingSlash}/`;
}
