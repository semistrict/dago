import type { FullConfig } from "@playwright/test";
import { spawn, spawnSync, type ChildProcessByStdio } from "node:child_process";
import { once } from "node:events";
import { copyFile, mkdir, mkdtemp, rm } from "node:fs/promises";
import { createServer } from "node:http";
import { homedir, tmpdir } from "node:os";
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

  const slowAPI = createServer(async (request, response) => {
    let raw = "";
    for await (const chunk of request) raw += chunk.toString();
    const body = JSON.parse(raw) as Record<string, unknown>;
    const longResponse = raw.includes("finish this response, then leave the transcript scrollable");
    const parallelTools = raw.includes("show each completed parallel tool immediately");
    const tokenProgress = raw.includes("TOKEN_PROGRESS_WORKER");
    const hasToolResults = raw.includes("function_call_output");
    if (tokenProgress && body.stream === true) {
      const first = "first visible worker progress ".repeat(12);
      const second = "second visible worker progress ".repeat(24);
      response.writeHead(200, { "cache-control": "no-cache", "content-type": "text/event-stream" });
      setTimeout(() => response.write(`data: ${JSON.stringify({ type: "response.output_text.delta", delta: first })}\n\n`), 1_000);
      setTimeout(() => response.write(`data: ${JSON.stringify({ type: "response.output_text.delta", delta: second })}\n\n`), 2_500);
      setTimeout(() => {
        const payload = {
          id: "response-token-progress",
          status: "completed",
          output: [{
            type: "message",
            id: "message-token-progress",
            role: "assistant",
            content: [{ type: "output_text", text: first + second }]
          }],
          usage: { input_tokens: 120, output_tokens: 180, total_tokens: 300 }
        };
        response.end(`data: ${JSON.stringify({ type: "response.completed", response: payload })}\n\n`);
      }, 4_000);
      return;
    }
    setTimeout(() => {
      const output = parallelTools && !hasToolResults
        ? [
            {
              type: "function_call",
              id: "item-parallel-ls",
              call_id: "parallel-ls",
              name: "ls",
              arguments: JSON.stringify({ path: "/" })
            },
            {
              type: "function_call",
              id: "item-parallel-read",
              call_id: "parallel-read",
              name: "read_file",
              arguments: JSON.stringify({ file_path: "/README.md", offset: 0, limit: 20 })
            },
            {
              type: "function_call",
              id: "item-parallel-execute",
              call_id: "parallel-execute",
              name: "execute",
              arguments: JSON.stringify({ command: "sleep 4", timeout: 10 })
            }
          ]
        : [{
            type: "message",
            id: "message-playwright",
            role: "assistant",
            content: [{
              type: "output_text",
              text: parallelTools
                ? "Parallel tool batch finished."
                : longResponse
                  ? Array.from({ length: 50 }, (_, index) => `response line ${index}`).join("\n")
                  : ""
            }]
          }];
      const payload = {
        id: "response-playwright",
        status: "completed",
        output,
        usage: { input_tokens: 1, output_tokens: longResponse ? 100 : 0, total_tokens: longResponse ? 101 : 1 }
      };
      if (body.stream === true) {
        response.writeHead(200, { "cache-control": "no-cache", "content-type": "text/event-stream" });
        response.end(`data: ${JSON.stringify({ type: "response.completed", response: payload })}\n\n`);
        return;
      }
      response.writeHead(200, { "content-type": "application/json" });
      response.end(JSON.stringify(payload));
    }, parallelTools ? 50 : 2_000);
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
  const workflowFixture = await startWorkflowFixture();

  const seedBinary = path.join(temporary, "sessionseed");
  const seedBuild = spawnSync("go", ["build", "-o", seedBinary, "./internal/dacode/xtermjs/testdata/sessionseed"], {
    cwd: root,
    encoding: "utf8"
  });
  if (seedBuild.status !== 0) {
    await rm(temporary, { force: true, recursive: true });
    throw new Error(`failed to build session fixture helper:\n${seedBuild.stderr}`);
  }
  for (const name of ["resume", "direct-resume"]) {
    const stateDirectory = path.join(temporary, `state-${name}`);
    await mkdir(stateDirectory, { recursive: true });
    const seed = spawnSync(seedBinary, [path.join(stateDirectory, "threads.db"), path.join(homedir(), "browser-fixture")], {
      encoding: "utf8"
    });
    if (seed.status !== 0) {
      await rm(temporary, { force: true, recursive: true });
      throw new Error(`failed to seed ${name} sessions:\n${seed.stderr}`);
    }
  }

  const servers = [
    startServer(binary, temporary, "default", slowAPIURL),
    startServer(binary, temporary, "manual", slowAPIURL, ["--manual-review"]),
    startServer(binary, temporary, "yolo", slowAPIURL, ["--yolo"]),
    startServer(binary, temporary, "resume", slowAPIURL, ["resume"]),
    startServer(binary, temporary, "direct-resume", slowAPIURL, ["resume", "playwright-newer"]),
    startServer(binary, temporary, "workflow-fake", workflowFixture.url, ["--approve-for-me"])
  ];

  const liveEnabled = process.env.DAGO_PLAYWRIGHT_OPENAI_LIVE === "1";
  if (liveEnabled) {
    const liveState = path.join(temporary, "state-openai-live");
    await mkdir(liveState, { recursive: true });
    const oauthFile = process.env.DAGO_OPENAI_OAUTH_FILE;
    if (oauthFile) {
      await copyFile(oauthFile, path.join(liveState, "openai-oauth.json"));
    } else if (!process.env.OPENAI_API_KEY) {
      throw new Error("DAGO_PLAYWRIGHT_OPENAI_LIVE requires DAGO_OPENAI_OAUTH_FILE or OPENAI_API_KEY");
    }
    const model = process.env.DAGO_OPENAI_LIVE_MODEL ?? "gpt-5.6-terra";
    servers.push(startServer(binary, temporary, "openai-live", "", ["--approve-for-me", "-M", model], true));
  }

  try {
    const urls = await Promise.all(servers.map(serverURL));
    const [baseURL, manualURL, yoloURL, resumeURL, directResumeURL, workflowFakeURL] = urls;
    process.env.PLAYWRIGHT_TEST_BASE_URL = baseURL;
    process.env.PLAYWRIGHT_MANUAL_URL = manualURL;
    process.env.PLAYWRIGHT_YOLO_URL = yoloURL;
    process.env.PLAYWRIGHT_RESUME_URL = resumeURL;
    process.env.PLAYWRIGHT_DIRECT_RESUME_URL = directResumeURL;
    process.env.PLAYWRIGHT_WORKFLOW_FAKE_URL = workflowFakeURL;
    process.env.PLAYWRIGHT_WORKFLOW_FAKE_API_URL = workflowFixture.url;
    if (liveEnabled) process.env.PLAYWRIGHT_OPENAI_LIVE_URL = urls[urls.length - 1];
  } catch (error) {
    for (const server of servers) server.kill("SIGTERM");
    slowAPI.closeAllConnections();
    slowAPI.close();
    workflowFixture.server.closeAllConnections();
    workflowFixture.server.close();
    await rm(temporary, { force: true, recursive: true });
    throw error;
  }

  return async () => {
    await Promise.all(servers.map(stopServer));
    slowAPI.closeAllConnections();
    await new Promise<void>((resolve) => slowAPI.close(() => resolve()));
    workflowFixture.server.closeAllConnections();
    await new Promise<void>((resolve) => workflowFixture.server.close(() => resolve()));
    await rm(temporary, { force: true, recursive: true });
  };
}

async function startWorkflowFixture() {
  const state = {
    approvalReviews: 0,
    chosenFailureCalls: 0,
    deniedApprovalReviews: 0,
    failedWorkerRequests: 0,
    foregroundWorkflowCalls: 0,
    recoveryAlternativeCalls: 0,
    recoveryContinuations: 0,
    scoutCalls: 0,
    workerExecuteCalls: 0,
    workerExecuteContinuations: 0
  };
  let sequence = 0;
  const server = createServer(async (request, response) => {
    if (request.method === "GET" && request.url === "/fixture-state") {
      response.writeHead(200, { "content-type": "application/json" });
      response.end(JSON.stringify(state));
      return;
    }

    let raw = "";
    for await (const chunk of request) raw += chunk.toString();
    const body = JSON.parse(raw) as Record<string, any>;
    const input = JSON.stringify(body.input ?? []);
    const instructions = String(body.instructions ?? "");
    const requestText = `${instructions}\n${input}`;
    const formatName = String(body.text?.format?.name ?? "");
    const id = `response-workflow-${++sequence}`;

    const send = (output: Record<string, any>[]) => {
      const payload = {
        id,
        status: "completed",
        output,
        usage: { input_tokens: 40, output_tokens: 20, total_tokens: 60 }
      };
      if (body.stream === true) {
        response.writeHead(200, {
          "cache-control": "no-cache",
          "content-type": "text/event-stream"
        });
        response.end(`data: ${JSON.stringify({ type: "response.completed", response: payload })}\n\n`);
        return;
      }
      response.writeHead(200, { "content-type": "application/json" });
      response.end(JSON.stringify(payload));
    };
    const sendText = (text: string) => send([{
      type: "message",
      id: `message-workflow-${sequence}`,
      role: "assistant",
      content: [{ type: "output_text", text }]
    }]);
    const sendTool = (callID: string, name: string, argumentsValue: Record<string, unknown>) => send([{
      type: "function_call",
      id: `item-workflow-${sequence}`,
      call_id: callID,
      name,
      arguments: JSON.stringify(argumentsValue)
    }]);

    if (formatName === "approval_assessment") {
      state.approvalReviews += 1;
      const denied = requestText.includes("DENY_BACKGROUND_ACTION");
      if (denied) state.deniedApprovalReviews += 1;
      if (requestText.includes("AUTO_REVIEW_VISIBILITY")) {
        await new Promise((resolve) => setTimeout(resolve, 600));
      }
      sendText(JSON.stringify({
        risk_level: denied ? "high" : "low",
        user_authorization: denied ? "low" : "high",
        outcome: denied ? "deny" : "allow",
        rationale: denied
          ? "The requested action is outside the read-only workflow scope; use a safe inspection instead."
          : "The read-only repository inspection is within the requested workflow scope."
      }));
      return;
    }

    if (instructions.includes("You are a workflow worker.")) {
      if (requestText.includes("FAIL_WORKER")) {
        state.failedWorkerRequests += 1;
        response.writeHead(400, { "content-type": "application/json" });
        response.end(JSON.stringify({
          error: { message: "synthetic terminal worker failure", type: "invalid_request_error" }
        }));
        return;
      }
      if (requestText.includes("RECOVER_AFTER_DENIAL")) {
        if (!input.includes("function_call_output")) {
          state.workerExecuteCalls += 1;
          sendTool(`recovery-denied-${sequence}`, "execute", { command: "DENY_BACKGROUND_ACTION --mutate-repository" });
          return;
        }
        if (!input.includes("recovery-read-")) {
          state.recoveryContinuations += 1;
          state.recoveryAlternativeCalls += 1;
          sendTool(`recovery-read-${sequence}`, "read_file", { file_path: "/docs/ARCHITECTURE.md", offset: 0, limit: 80 });
          return;
        }
        state.recoveryContinuations += 1;
        sendText(JSON.stringify({ findings: [{
          title: "Keep denied workflow workers alive for safe alternatives",
          files: ["internal/dacode/workflows.go"],
          problem: "Approval denial must be returned to the worker instead of terminating its branch.",
          refactor: "Preserve the approval resume loop and make terminal failure an explicit worker choice.",
          impact: "high",
          effort: "small",
          evidence: "The worker recovered from a denied execute call by reading docs/ARCHITECTURE.md."
        }] }));
        return;
      }
      if (requestText.includes("CHOOSE_FAIL_AFTER_DENIAL")) {
        if (!input.includes("function_call_output")) {
          state.workerExecuteCalls += 1;
          sendTool(`chosen-failure-denied-${sequence}`, "execute", { command: "DENY_BACKGROUND_ACTION --required-for-task" });
          return;
        }
        state.chosenFailureCalls += 1;
        sendTool(`chosen-failure-${sequence}`, "fail_workflow_agent", {
          reason: "The required action was denied and no safe alternative can satisfy this assigned branch."
        });
        return;
      }
      if (!input.includes("function_call_output")) {
        state.workerExecuteCalls += 1;
        sendTool(`worker-execute-${sequence}`, "execute", { command: "git status --short" });
        return;
      }
      state.workerExecuteContinuations += 1;
      if (requestText.includes("SCAN_CORE")) {
        sendText(JSON.stringify({ findings: [{
          title: "Consolidate workflow lifecycle transitions",
          files: ["daworkflow/manager.go", "daworkflow/runtime.go"],
          problem: "Lifecycle state and terminal failure accounting span the manager and runtime.",
          refactor: "Introduce one internal transition helper with explicit invariants.",
          impact: "high",
          effort: "medium",
          evidence: "daworkflow/manager.go owns terminal state while daworkflow/runtime.go emits agent_failed events."
        }] }));
        return;
      }
      if (requestText.includes("SCAN_TUI")) {
        sendText(JSON.stringify({ findings: [{
          title: "Separate workflow panel projection from rendering",
          files: ["internal/dacode/workflow_tui.go"],
          problem: "Event aggregation and terminal layout are coupled in one panel implementation.",
          refactor: "Extract a tested run-view projection consumed by the renderer.",
          impact: "medium",
          effort: "small",
          evidence: "internal/dacode/workflow_tui.go computes event counts beside terminal presentation code."
        }] }));
        return;
      }
      if (requestText.includes("VERIFY_FINDING")) {
        sendText(JSON.stringify({
          verdict: "confirmed",
          reasoning: "The cited files contain the stated cross-layer responsibility and the proposed boundary preserves behavior.",
          impact: "high",
          effort: "medium",
          sketch: "Add an internal projection or transition type, migrate callers, then retain contract tests."
        }));
        return;
      }
      if (requestText.includes("SYNTHESIZE_REPORT")) {
        sendText("Prioritized report: first consolidate workflow lifecycle transitions, then extract the TUI run projection. One scanner failed, so the report explicitly records incomplete coverage.");
        return;
      }
      sendText("Repository evidence collected.");
      return;
    }

    if (requestText.includes("AUTO_REVIEW_VISIBILITY")) {
      if (!input.includes("function_call_output")) {
        sendTool(`visibility-execute-${sequence}`, "execute", { command: "printf AUTO_REVIEW_VISIBILITY" });
        return;
      }
      sendText("Automatic review completed without displaying manual controls.");
      return;
    }

    if (requestText.includes("<workflow_notification>")) {
      sendText("Partial workflow result reviewed: three refactoring findings were independently verified; two scanners failed, so coverage is explicitly incomplete.");
      return;
    }
    if (input.includes("workflow-start")) {
      sendText("Started the grounded repository refactoring workflow. I will report its completion and any worker failures.");
      return;
    }
    if (input.includes("scout-read")) {
      state.foregroundWorkflowCalls += 1;
      sendTool("workflow-start", "workflow", { script: realisticWorkflowScript() });
      return;
    }

    state.scoutCalls += 1;
    sendTool("scout-read", "read_file", { file_path: "/docs/ARCHITECTURE.md", offset: 0, limit: 120 });
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  if (!address || typeof address === "string") {
    server.close();
    throw new Error("failed to start workflow Responses API fixture");
  }
  return { server, url: `http://127.0.0.1:${address.port}` };
}

function realisticWorkflowScript(): string {
  return `export const meta = {
  name: 'repository-refactoring-map',
  description: 'Grounded parallel repository review with adversarial verification and explicit partial-failure handling',
  phases: [
    { title: 'Scan', detail: 'Independent package analysis' },
    { title: 'Verify', detail: 'Adversarial evidence checks' },
    { title: 'Synthesize', detail: 'Prioritized final map' },
  ],
}

const FINDINGS = {
  type: 'object',
  additionalProperties: false,
  required: ['findings'],
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        additionalProperties: false,
        required: ['title', 'files', 'problem', 'refactor', 'impact', 'effort', 'evidence'],
        properties: {
          title: { type: 'string' },
          files: { type: 'array', items: { type: 'string' } },
          problem: { type: 'string' },
          refactor: { type: 'string' },
          impact: { enum: ['high', 'medium', 'low'] },
          effort: { enum: ['small', 'medium', 'large'] },
          evidence: { type: 'string' },
        },
      },
    },
  },
}

const VERDICT = {
  type: 'object',
  additionalProperties: false,
  required: ['verdict', 'reasoning', 'impact', 'effort', 'sketch'],
  properties: {
    verdict: { enum: ['confirmed', 'partial', 'rejected'] },
    reasoning: { type: 'string' },
    impact: { enum: ['high', 'medium', 'low'] },
    effort: { enum: ['small', 'medium', 'large'] },
    sketch: { type: 'string' },
  },
}

phase('Scan')
const scans = await parallel([
  () => agent('SCAN_CORE: inspect workflow runtime and manager boundaries read-only; cite concrete files.', { label: 'scan:core', phase: 'Scan', schema: FINDINGS }),
  () => agent('SCAN_TUI: inspect workflow TUI state and rendering boundaries read-only; cite concrete files.', { label: 'scan:tui', phase: 'Scan', schema: FINDINGS }),
  () => agent('RECOVER_AFTER_DENIAL: request the initial action, then use a safe alternative if it is denied.', { label: 'scan:denial-recovery', phase: 'Scan', schema: FINDINGS }),
  () => agent('CHOOSE_FAIL_AFTER_DENIAL: if the required action is denied and no safe alternative exists, explicitly fail this branch.', { label: 'scan:chosen-failure', phase: 'Scan', schema: FINDINGS }),
  () => agent('FAIL_WORKER: inspect provider integration boundaries read-only; cite concrete files.', { label: 'scan:providers', phase: 'Scan', schema: FINDINGS }),
])
const findings = scans.flatMap(result => result?.findings ?? [])
log(findings.length + ' findings from ' + scans.filter(Boolean).length + '/5 scanners')
if (findings.length === 0) throw new Error('all scanners failed; no usable findings')

phase('Verify')
const verified = await parallel(findings.map((finding, index) => () =>
  agent('VERIFY_FINDING: try to refute this finding against the repository.\\n' + JSON.stringify(finding), {
    label: 'verify:' + index,
    phase: 'Verify',
    schema: VERDICT,
  }).then(verdict => verdict ? { finding, verdict } : null)))
const usable = verified.filter(Boolean)
if (usable.length === 0) throw new Error('all verification workers failed')

phase('Synthesize')
const report = await agent('SYNTHESIZE_REPORT: prioritize these verified refactors and disclose incomplete scan coverage.\\n' + JSON.stringify(usable), {
  label: 'synthesize-refactoring-map',
  phase: 'Synthesize',
})
if (!report) throw new Error('synthesis failed')
return { report, findings: usable, failedScanners: scans.length - scans.filter(Boolean).length }
`;
}

function startServer(
  binary: string,
  temporary: string,
  name: string,
  baseURL: string,
  extraArguments: string[] = [],
  live = false
) {
  const command = extraArguments[0] === "resume" ? ["resume"] : [];
  const options = command.length > 0 ? extraArguments.slice(1) : extraArguments;
  return spawn(
    binary,
    [
      ...command,
      "--serve-xtermjs",
      "--xtermjs-address",
      "127.0.0.1:0",
      "--state-dir",
      path.join(temporary, `state-${name}`),
      "--cwd",
      root,
      ...options
    ],
    {
      cwd: root,
      env: live
        ? { ...process.env, OPENAI_BASE_URL: "", TERM: "xterm-256color" }
        : {
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
