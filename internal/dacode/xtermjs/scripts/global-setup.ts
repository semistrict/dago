import type { FullConfig } from "@playwright/test";
import { spawn, spawnSync, type ChildProcessByStdio } from "node:child_process";
import { createHash } from "node:crypto";
import { once } from "node:events";
import { copyFile, mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../..");

async function availableLoopbackAddress(): Promise<string> {
  const server = createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  if (!address || typeof address === "string") {
    server.close();
    throw new Error("failed to reserve a loopback address");
  }
  await new Promise<void>((resolve) => server.close(() => resolve()));
  return `127.0.0.1:${address.port}`;
}

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

  const editorBinary = path.join(temporary, "editorfixture");
  const editorBuild = spawnSync("go", ["build", "-o", editorBinary, "./internal/dacode/xtermjs/testdata/editorfixture"], {
    cwd: root,
    encoding: "utf8"
  });
  if (editorBuild.status !== 0) {
    await rm(temporary, { force: true, recursive: true });
    throw new Error(`failed to build editor fixture helper:\n${editorBuild.stderr}`);
  }

  const unsupportedEffortBinary = path.join(temporary, "effortfixture");
  const unsupportedEffortBuild = spawnSync(
    "go",
    ["build", "-tags", "dacode_e2e_fixture", "-o", unsupportedEffortBinary, "./internal/dacode/xtermjs/testdata/effortfixture"],
    { cwd: root, encoding: "utf8" }
  );
  if (unsupportedEffortBuild.status !== 0) {
    await rm(temporary, { force: true, recursive: true });
    throw new Error(`failed to build unsupported effort fixture helper:\n${unsupportedEffortBuild.stderr}`);
  }

  const authMCPAutoBinary = path.join(temporary, "authmcpautofixture");
  const authMCPAutoBuild = spawnSync(
    "go",
    ["build", "-tags", "dacode_e2e_fixture", "-o", authMCPAutoBinary, "./internal/dacode/xtermjs/testdata/authmcpautofixture"],
    { cwd: root, encoding: "utf8" }
  );
  if (authMCPAutoBuild.status !== 0) {
    await rm(temporary, { force: true, recursive: true });
    throw new Error(`failed to build auth/MCP/Auto fixture helper:\n${authMCPAutoBuild.stderr}`);
  }

  const notificationUpdateTraceBinary = path.join(temporary, "notificationupdatetracefixture");
  const notificationUpdateTraceBuild = spawnSync(
    "go",
    ["build", "-tags", "dacode_e2e_fixture", "-o", notificationUpdateTraceBinary, "./internal/dacode/xtermjs/testdata/notificationupdatetracefixture"],
    { cwd: root, encoding: "utf8" }
  );
  if (notificationUpdateTraceBuild.status !== 0) {
    await rm(temporary, { force: true, recursive: true });
    throw new Error(`failed to build notification/update/trace fixture helper:\n${notificationUpdateTraceBuild.stderr}`);
  }

  const interactionBinary = path.join(temporary, "interactionfixture");
  const interactionBuild = spawnSync(
    "go",
    ["build", "-tags", "dacode_e2e_fixture", "-o", interactionBinary, "./internal/dacode/xtermjs/testdata/interactionfixture"],
    { cwd: root, encoding: "utf8" }
  );
  if (interactionBuild.status !== 0) {
    await rm(temporary, { force: true, recursive: true });
    throw new Error(`failed to build interaction fixture helper:\n${interactionBuild.stderr}`);
  }

  const polishBinary = path.join(temporary, "polishfixture");
  const polishBuild = spawnSync(
    "go",
    ["build", "-tags", "dacode_e2e_fixture", "-o", polishBinary, "./internal/dacode/xtermjs/testdata/polishfixture"],
    { cwd: root, encoding: "utf8" }
  );
  if (polishBuild.status !== 0) {
    await rm(temporary, { force: true, recursive: true });
    throw new Error(`failed to build polish fixture helper:\n${polishBuild.stderr}`);
  }

  const restartBinary = path.join(temporary, "restartfixture");
  const restartBuild = spawnSync("go", ["build", "-o", restartBinary, "./internal/dacode/xtermjs/testdata/restartfixture"], {
    cwd: root,
    encoding: "utf8"
  });
  if (restartBuild.status !== 0) {
    await rm(temporary, { force: true, recursive: true });
    throw new Error(`failed to build restart fixture helper:\n${restartBuild.stderr}`);
  }

  let transientRetryAttempts = 0;
  let activeGoalInstructions: string | undefined;
  let localContextInstructions: string | undefined;
  const slowAPI = createServer((request, response) => {
    let body = "";
    request.setEncoding("utf8");
    request.on("data", (chunk: string) => {
      body += chunk;
    });
    request.on("end", () => {
	  let requestPayload: { instructions?: string } = {};
	  try {
		requestPayload = JSON.parse(body) as { instructions?: string };
	  } catch {
		// Individual response branches retain their existing malformed-request behavior.
	  }
      const longResponse = body.includes("finish this response, then leave the transcript scrollable");
      const parallelTools = body.includes("show each completed parallel tool immediately");
      const hasToolResults = body.includes("function_call_output");
      return (
      setTimeout(() => {
		if (body.includes("retry a transient model transport failure")) {
		  transientRetryAttempts += 1;
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  if (transientRetryAttempts === 1) {
			response.end();
			return;
		  }
		  const completed = {
			type: "response.completed",
			response: {
			  id: "response-transient-retry",
			  status: "completed",
			  output: [{ type: "message", id: "message-transient-retry", role: "assistant", content: [{ type: "output_text", text: "Transient model transport recovered automatically." }] }],
			  usage: { input_tokens: 1, output_tokens: 5, total_tokens: 6 }
			}
		  };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("TOKEN_PROGRESS_WORKER")) {
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
		if (requestPayload.instructions?.includes("Prove active goal cache stability")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  if (!hasToolResults) {
			activeGoalInstructions = requestPayload.instructions;
			const completed = {
			  type: "response.completed",
			  response: {
				id: "response-goal-cache-tool",
				status: "completed",
				output: [{ type: "function_call", id: "item-goal-cache-ls", call_id: "goal-cache-ls", name: "ls", arguments: JSON.stringify({ path: "." }) }],
				usage: { input_tokens: 20, output_tokens: 2, total_tokens: 22 }
			  }
			};
			response.end(`data: ${JSON.stringify(completed)}\n\n`);
			return;
		  }
		  const stable = activeGoalInstructions !== undefined && requestPayload.instructions === activeGoalInstructions;
		  const completed = {
			type: "response.completed",
			response: {
			  id: "response-goal-cache-result",
			  status: "completed",
			  output: [{ type: "message", id: "message-goal-cache-result", role: "assistant", content: [{ type: "output_text", text: stable ? "Active-goal system prompt remained cache-stable." : "Active-goal system prompt changed between model calls." }] }],
			  usage: { input_tokens: 24, output_tokens: 6, total_tokens: 30 }
			}
		  };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("create a local context cache probe") && !body.includes("verify the local context cache snapshot")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  if (!hasToolResults) {
			localContextInstructions = requestPayload.instructions;
			const completed = {
			  type: "response.completed",
			  response: {
				id: "response-local-context-cache-tool",
				status: "completed",
				output: [{ type: "function_call", id: "item-local-context-cache-write", call_id: "local-context-cache-write", name: "write_file", arguments: JSON.stringify({ file_path: "local-context-cache-probe.txt", content: "created after the session snapshot\n" }) }],
				usage: { input_tokens: 20, output_tokens: 2, total_tokens: 22 }
			  }
			};
			response.end(`data: ${JSON.stringify(completed)}\n\n`);
			return;
		  }
		  const completed = {
			type: "response.completed",
			response: {
			  id: "response-local-context-cache-prepared",
			  status: "completed",
			  output: [{ type: "message", id: "message-local-context-cache-prepared", role: "assistant", content: [{ type: "output_text", text: "Local context cache probe prepared." }] }],
			  usage: { input_tokens: 24, output_tokens: 4, total_tokens: 28 }
			}
		  };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("verify the local context cache snapshot")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const stable = localContextInstructions !== undefined && requestPayload.instructions === localContextInstructions;
		  const completed = {
			type: "response.completed",
			response: {
			  id: "response-local-context-cache-result",
			  status: "completed",
			  output: [{ type: "message", id: "message-local-context-cache-result", role: "assistant", content: [{ type: "output_text", text: stable ? "Local-context system prompt remained cache-stable across turns." : "Local-context system prompt changed across turns." }] }],
			  usage: { input_tokens: 24, output_tokens: 6, total_tokens: 30 }
			}
		  };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (parallelTools && !hasToolResults) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = {
			type: "response.completed",
			response: {
			  id: "response-parallel-tools",
			  status: "completed",
			  output: [
				{ type: "function_call", id: "item-parallel-ls", call_id: "parallel-ls", name: "ls", arguments: JSON.stringify({ path: "." }) },
				{ type: "function_call", id: "item-parallel-read", call_id: "parallel-read", name: "read_file", arguments: JSON.stringify({ file_path: "parallel-fixture.txt", offset: 0, limit: 20 }) },
				{ type: "function_call", id: "item-parallel-execute", call_id: "parallel-execute", name: "execute", arguments: JSON.stringify({ command: "sleep 4", timeout: 10 }) }
			  ],
			  usage: { input_tokens: 1, output_tokens: 3, total_tokens: 4 }
			}
		  };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (parallelTools || longResponse) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const text = parallelTools
			? "Parallel tool batch finished."
			: Array.from({ length: 50 }, (_, index) => `response line ${index}`).join("\n");
		  const completed = {
			type: "response.completed",
			response: {
			  id: parallelTools ? "response-parallel-complete" : "response-long",
			  status: "completed",
			  output: [{ type: "message", id: "message-playwright", role: "assistant", content: [{ type: "output_text", text }] }],
			  usage: { input_tokens: 1, output_tokens: longResponse ? 100 : 1, total_tokens: longResponse ? 101 : 2 }
			}
		  };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("Summarize the earlier conversation faithfully.")) {
		  const result = {
			id: "response-compaction-summary",
			status: "completed",
			output: [{
			  type: "message",
			  id: "message-compaction-summary",
			  role: "assistant",
			  content: [{ type: "output_text", text: "Playwright conversation summary." }]
			}],
			usage: { input_tokens: 4, output_tokens: 2, total_tokens: 6 }
		  };
		  response.writeHead(200, { "content-type": "application/json" });
		  response.end(JSON.stringify(result));
		  return;
		}
		if (body.includes("GoalProposal")) {
		  const revised = body.includes("make the browser check explicit");
		  const cacheStability = body.includes("Prove active goal cache stability");
		  const proposal = {
			objective: cacheStability ? "Prove active goal cache stability" : "Finish the release checklist",
			criteria: revised
			  ? "- Release checklist is complete.\n- Browser verification passes."
			  : "- Release checklist is complete.\n- Verification passes."
		  };
		  const result = { id: revised ? "response-goal-revised" : "response-goal-proposal", status: "completed", output: [{ type: "message", id: "message-goal-proposal", role: "assistant", content: [{ type: "output_text", text: JSON.stringify(proposal) }] }], usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 } };
		  response.writeHead(200, { "content-type": "application/json" });
		  response.end(JSON.stringify(result));
		  return;
		}
		if (body.includes("GraderResponse")) {
		  const grading = {
			result: "satisfied",
			explanation: "All acceptance criteria passed.",
			criteria: [
			  { name: "Release checklist is complete.", passed: true },
			  { name: "Verification passes.", passed: true }
			]
		  };
		  const result = { id: "response-rubric-grader", status: "completed", output: [{ type: "message", id: "message-rubric-grader", role: "assistant", content: [{ type: "output_text", text: JSON.stringify(grading) }] }], usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 } };
		  response.writeHead(200, { "content-type": "application/json" });
		  response.end(JSON.stringify(result));
		  return;
		}
		if (body.includes("I'm invoking the skill `remember`") && body.includes("User request: browser learning") && !body.includes("User request: browser workflow") && !body.includes("User request: latest turn")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = { type: "response.completed", response: { id: "response-remember-skill", status: "completed", output: [{ type: "message", id: "message-remember-skill", role: "assistant", content: [{ type: "output_text", text: "Remember skill invoked." }] }], usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 } } };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("I'm invoking the skill `skill-creator`") && body.includes("User request: browser workflow") && !body.includes("User request: latest turn")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = { type: "response.completed", response: { id: "response-creator-skill", status: "completed", output: [{ type: "message", id: "message-creator-skill", role: "assistant", content: [{ type: "output_text", text: "Skill creator invoked." }] }], usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 } } };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("I'm invoking the skill `deepagents-thread-inspector`") && body.includes("User request: latest turn")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = { type: "response.completed", response: { id: "response-inspector-skill", status: "completed", output: [{ type: "message", id: "message-inspector-skill", role: "assistant", content: [{ type: "output_text", text: "Thread inspector invoked." }] }], usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 } } };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("I'm invoking the skill `playwright-external`")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = { type: "response.completed", response: { id: "response-external-skill", status: "completed", output: [{ type: "message", id: "message-external-skill", role: "assistant", content: [{ type: "output_text", text: "External skill invoked." }] }], usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 } } };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("show subagent fanout") && body.includes("function_call_output")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = { type: "response.completed", response: { id: "response-fanout-complete", status: "completed", output: [{ type: "message", id: "message-fanout-complete", role: "assistant", content: [{ type: "output_text", text: "Fan-out complete." }] }], usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 } } };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("show subagent fanout")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = { type: "response.completed", response: { id: "response-fanout-start", status: "completed", output: [
		    { type: "function_call", id: "item-fanout-alpha", call_id: "call-fanout-alpha", name: "task", arguments: JSON.stringify({ description: "fanout child alpha", subagent_type: "general-purpose" }) },
		    { type: "function_call", id: "item-fanout-beta", call_id: "call-fanout-beta", name: "task", arguments: JSON.stringify({ description: "fanout child beta", subagent_type: "general-purpose" }) }
		  ], usage: { input_tokens: 1, output_tokens: 2, total_tokens: 3 } } };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("fanout child alpha") || body.includes("fanout child beta")) {
		  const label = body.includes("fanout child alpha") ? "alpha" : "beta";
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = { type: "response.completed", response: { id: `response-fanout-${label}`, status: "completed", output: [{ type: "message", id: `message-fanout-${label}`, role: "assistant", content: [{ type: "output_text", text: `${label} child complete` }] }], usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("render second diff") && body.includes("call-transcript-second") && body.includes("function_call_output")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = { type: "response.completed", response: { id: "response-transcript-second-complete", status: "completed", output: [{ type: "message", id: "message-transcript-second", role: "assistant", content: [{ type: "output_text", text: "Second diff complete." }] }], usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 } } };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("render second diff")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = { type: "response.completed", response: { id: "response-transcript-second", status: "completed", output: [{ type: "function_call", id: "item-transcript-second", call_id: "call-transcript-second", name: "write_file", arguments: JSON.stringify({ file_path: path.join(transcriptWorkspace, "transcript-second.txt"), content: "gamma\ndelta\n" }) }], usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("render transcript tools") && body.includes("call-transcript-write-two") && body.includes("function_call_output")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const markdownStart = "# Transcript rendering\n\n- Glamour list item\n- [Glamour link](https://example.com/glamour)\n\n| Renderer | State |\n| --- | --- |\n| Glamour | active |\n\n";
		  response.write(`data: ${JSON.stringify({ type: "response.output_text.delta", delta: markdownStart })}\n\n`);
		  setTimeout(() => {
		    response.write(`data: ${JSON.stringify({ type: "response.output_text.delta", delta: "**streamed markdown** with `inline code`." })}\n\n`);
		    const completed = { type: "response.completed", response: { id: "response-transcript-complete", status: "completed", output: [{ type: "message", id: "message-transcript", role: "assistant", content: [{ type: "output_text", text: markdownStart + "**streamed markdown** with `inline code`." }] }], usage: { input_tokens: 3, output_tokens: 4, total_tokens: 7 } } };
		    response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  }, 1_000);
		  return;
		}
		if (body.includes("render transcript tools")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = { type: "response.completed", response: { id: "response-transcript-tools", status: "completed", output: [
		    { type: "function_call", id: "item-transcript-write-one", call_id: "call-transcript-write-one", name: "write_file", arguments: JSON.stringify({ file_path: path.join(transcriptWorkspace, "transcript-one.txt"), content: "alpha\nbeta\n" }) },
		    { type: "function_call", id: "item-transcript-write-two", call_id: "call-transcript-write-two", name: "write_file", arguments: JSON.stringify({ file_path: path.join(transcriptWorkspace, "transcript-two.txt"), content: "one\ntwo\n" }) }
		  ], usage: { input_tokens: 1, output_tokens: 2, total_tokens: 3 } } };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
		if (body.includes("long transcript user")) {
		  response.writeHead(200, { "content-type": "text/event-stream" });
		  const completed = { type: "response.completed", response: { id: "response-long-user", status: "completed", output: [{ type: "message", id: "message-long-user", role: "assistant", content: [{ type: "output_text", text: "Long user message received." }] }], usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 } } };
		  response.end(`data: ${JSON.stringify(completed)}\n\n`);
		  return;
		}
        if (body.includes(">>> APPROVAL REQUEST START") && body.includes("deferred auto approval")) {
          response.writeHead(200, { "content-type": "text/event-stream" });
          const completed = {
            type: "response.completed",
            response: {
              id: "response-deferred-auto-review-failed",
              status: "completed",
              output: [
                {
                  type: "message",
                  id: "message-deferred-auto-review-failed",
                  role: "assistant",
                  content: [{ type: "output_text", text: "not an approval assessment" }]
                }
              ],
              usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 }
            }
          };
          response.end(`data: ${JSON.stringify(completed)}\n\n`);
          return;
        }
        if (body.includes("User rejected the tool call with reason: use safer read check")) {
          response.writeHead(200, { "content-type": "text/event-stream" });
          const completed = {
            type: "response.completed",
            response: {
              id: "response-reasoned-rejection-finished",
              status: "completed",
              output: [
                {
                  type: "message",
                  id: "message-reasoned-rejection-finished",
                  role: "assistant",
                  content: [{ type: "output_text", text: "Rejection feedback received." }]
                }
              ],
              usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 }
            }
          };
          response.end(`data: ${JSON.stringify(completed)}\n\n`);
          return;
        }
        if (body.includes("Rejected by user.")) {
          response.writeHead(200, { "content-type": "text/event-stream" });
          const completed = {
            type: "response.completed",
            response: {
              id: "response-bare-rejection-finished",
              status: "completed",
              output: [
                {
                  type: "message",
                  id: "message-bare-rejection-finished",
                  role: "assistant",
                  content: [{ type: "output_text", text: "Bare rejection received." }]
                }
              ],
              usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 }
            }
          };
          response.end(`data: ${JSON.stringify(completed)}\n\n`);
          return;
        }
        if (body.includes("Q: Project name?")) {
          response.writeHead(200, { "content-type": "text/event-stream" });
          const completed = {
            type: "response.completed",
            response: {
              id: "response-ask-user-finished",
              status: "completed",
              output: [
                {
                  type: "message",
                  id: "message-ask-user-finished",
                  role: "assistant",
                  content: [{ type: "output_text", text: "Answers received." }]
                }
              ],
              usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 }
            }
          };
          response.end(`data: ${JSON.stringify(completed)}\n\n`);
          return;
        }
        if (body.includes("show approval quick keys") && body.includes("function_call_output")) {
          response.writeHead(200, { "content-type": "text/event-stream" });
          const completed = {
            type: "response.completed",
            response: {
              id: "response-approved-action-finished",
              status: "completed",
              output: [{ type: "message", id: "message-approved-action-finished", role: "assistant", content: [{ type: "output_text", text: "Approved action completed." }] }],
              usage: { input_tokens: 2, output_tokens: 1, total_tokens: 3 }
            }
          };
          response.end(`data: ${JSON.stringify(completed)}\n\n`);
          return;
        }
        const approvalFixtureCalls: Record<string, Array<{ name: string; arguments: Record<string, unknown> }>> = {
          "show write approval": [{ name: "write_file", arguments: { file_path: path.join(approvalWorkspace, "preview.md"), content: "first line\nsecond line" } }],
          "show edit approval": [{ name: "edit_file", arguments: { file_path: path.join(approvalWorkspace, "preview.md"), old_string: "old value", new_string: "new value" } }],
          "show delete approval": [{ name: "delete", arguments: { file_path: path.join(approvalWorkspace, "preview.md") } }],
          "show sensitive write approval": [{ name: "write_file", arguments: { file_path: path.join(approvalWorkspace, ".env.local"), content: "SECRET=not-for-scrollback" } }],
          "show generic approval": [{ name: "web_search", arguments: { query: "current release status", max_results: 3 } }],
          "show long approval": [{ name: "execute", arguments: { command: "printf one; printf two; printf three; printf four; printf five; printf six; printf seven; printf eight; printf nine; printf ten; printf eleven; printf twelve" } }],
          "show batch approval": [
            { name: "write_file", arguments: { file_path: path.join(approvalWorkspace, "batch.md"), content: "batch content" } },
            { name: "execute", arguments: { command: "printf batch" } }
          ],
          "show approval quick keys": [{ name: "execute", arguments: { command: "printf approved" } }]
        };
        const approvalFixture = Object.entries(approvalFixtureCalls).find(([prompt]) => body.includes(prompt));
        if (approvalFixture) {
          response.writeHead(200, { "content-type": "text/event-stream" });
          const completed = {
            type: "response.completed",
            response: {
              id: "response-approval-widget",
              status: "completed",
              output: approvalFixture[1].map((call, index) => ({
                type: "function_call",
                id: `item-approval-widget-${index}`,
                call_id: `call-approval-widget-${index}`,
                name: call.name,
                arguments: JSON.stringify(call.arguments)
              })),
              usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 }
            }
          };
          response.end(`data: ${JSON.stringify(completed)}\n\n`);
          return;
        }
        if (body.includes("show ask user browser flow")) {
          response.writeHead(200, { "content-type": "text/event-stream" });
          const completed = {
            type: "response.completed",
            response: {
              id: "response-ask-user",
              status: "completed",
              output: [
                {
                  type: "function_call",
                  id: "item-ask-user",
                  call_id: "call-ask-user",
                  name: "ask_user",
                  arguments: JSON.stringify({
                    questions: [
                      { question: "Project name? (required)", type: "text" },
                      {
                        question: "Pick a color",
                        type: "multiple_choice",
                        choices: [{ value: "red" }, { value: "blue" }]
                      },
                      { question: "Optional detail", type: "text", required: false }
                    ]
                  })
                }
              ],
              usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 }
            }
          };
          response.end(`data: ${JSON.stringify(completed)}\n\n`);
          return;
        }
        if (body.includes("show security approval")) {
          response.writeHead(200, { "content-type": "text/event-stream" });
          const completed = {
            type: "response.completed",
            response: {
              id: "response-security-approval",
              status: "completed",
              output: [
                {
                  type: "function_call",
                  id: "item-security-approval",
                  call_id: "call-security-approval",
                  name: "execute",
                  arguments: JSON.stringify({ command: "printf safe\u202etext" })
                }
              ],
              usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 }
            }
          };
          response.end(`data: ${JSON.stringify(completed)}\n\n`);
          return;
        }
        if (body.includes("show reasoned approval") || body.includes("show blank reason approval")) {
          response.writeHead(200, { "content-type": "text/event-stream" });
          const completed = {
            type: "response.completed",
            response: {
              id: "response-reasoned-approval",
              status: "completed",
              output: [
                {
                  type: "function_call",
                  id: "item-reasoned-approval",
                  call_id: "call-reasoned-approval",
                  name: "execute",
                  arguments: JSON.stringify({ command: "printf approval" })
                }
              ],
              usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 }
            }
          };
          response.end(`data: ${JSON.stringify(completed)}\n\n`);
          return;
        }
        if (body.includes("show deferred approval") || body.includes("deferred auto approval")) {
          response.writeHead(200, { "content-type": "text/event-stream" });
          const deferredSuffix = body.includes("third deferred auto approval")
            ? "-third"
            : body.includes("second deferred auto approval")
              ? "-second"
              : "";
          const completed = {
            type: "response.completed",
            response: {
              id: `response-deferred-approval${deferredSuffix}`,
              status: "completed",
              output: [
                {
                  type: "function_call",
                  id: `item-deferred-approval${deferredSuffix}`,
                  call_id: `call-deferred-approval${deferredSuffix}`,
                  name: "execute",
                  arguments: JSON.stringify({ command: "printf deferred" })
                }
              ],
              usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 }
            }
          };
          response.end(`data: ${JSON.stringify(completed)}\n\n`);
          return;
        }
        response.writeHead(200, { "content-type": "text/event-stream" });
        const completed = {
          type: "response.completed",
          response: {
            id: "response-playwright",
            status: "completed",
            output: [{ type: "message", id: "message-playwright", role: "assistant", content: [] }],
            usage: { input_tokens: 1, output_tokens: 0, total_tokens: 1, cost: 0.0012 }
          }
        };
		response.end(`data: ${JSON.stringify(completed)}\n\n`);
		}, parallelTools || body.includes("show deferred approval") || body.includes("deferred auto approval") || body.includes("GoalProposal") || body.includes("GraderResponse") || body.includes("render transcript") || body.includes("long transcript user") || body.includes("show subagent fanout") ? 50 : 2_000)
      );
    });
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

  const failingAPI = createServer((_request, response) => {
    response.writeHead(500, { "content-type": "application/json" });
    response.end(JSON.stringify({ error: { message: "fixture summary unavailable" } }));
  });
  failingAPI.listen(0, "127.0.0.1");
  await once(failingAPI, "listening");
  const failingAddress = failingAPI.address();
  if (!failingAddress || typeof failingAddress === "string") {
    failingAPI.close();
    slowAPI.close();
    await rm(temporary, { force: true, recursive: true });
    throw new Error("failed to start failing Responses API fixture");
  }
  const failingAPIURL = `http://127.0.0.1:${failingAddress.port}`;

  const seedBinary = path.join(temporary, "sessionseed");
  const seedBuild = spawnSync("go", ["build", "-o", seedBinary, "./internal/dacode/xtermjs/testdata/sessionseed"], {
    cwd: root,
    encoding: "utf8"
  });
  if (seedBuild.status !== 0) {
    await rm(temporary, { force: true, recursive: true });
    throw new Error(`failed to build session fixture helper:\n${seedBuild.stderr}`);
  }
  const resumeOriginalWorkspace = path.join(temporary, "workspace-resume-original");
  await mkdir(resumeOriginalWorkspace, { recursive: true });
  for (const [name, mode, directory] of [
    ["resume", "", root],
    ["direct-resume", "", root],
    ["hostile", "hostile", root],
    ["transcript-history", "virtualized", root],
    ["approval-persist", "", root],
    ["approval-invalid", "", root],
    ["resume-flow", "lifecycle", resumeOriginalWorkspace],
    ["offload", "offload", root],
    ["offload-error", "offload", root]
  ] as const) {
    const stateDirectory = path.join(temporary, `state-${name}`);
    await mkdir(stateDirectory, { recursive: true });
    const seedArguments = [path.join(stateDirectory, "threads.db"), directory];
    if (mode) seedArguments.push(mode);
    const seed = spawnSync(seedBinary, seedArguments, { encoding: "utf8" });
    if (seed.status !== 0) {
      await rm(temporary, { force: true, recursive: true });
      throw new Error(`failed to seed ${name} sessions:\n${seed.stderr}`);
    }
  }

  const lifecycleAgent = path.join(temporary, "state-resume-flow", "builder");
  await mkdir(path.join(lifecycleAgent, "sessions"), { recursive: true, mode: 0o700 });
  await mkdir(path.join(lifecycleAgent, "skills"), { recursive: true, mode: 0o700 });
  await writeFile(path.join(lifecycleAgent, "AGENTS.md"), "Build carefully and verify the result.\n", { mode: 0o600 });
  await writeFile(path.join(lifecycleAgent, "sessions", "generation"), "0123456789abcdef01234567\n", { mode: 0o600 });

  const browserAgentDirectory = path.join(temporary, "state-default", "research");
  await mkdir(browserAgentDirectory, { recursive: true });
  await writeFile(path.join(browserAgentDirectory, "AGENTS.md"), "Research carefully and report sources.\n", {
    mode: 0o600
  });

  for (const name of ["default", "manual", "resume", "direct-resume", "resume-flow", "offload", "offload-error", "onboarding", "restart-success", "restart-error", "restart-unavailable", "hostile", "transcript", "editor", "effort", "startup", "input", "ask-user", "ask-user-cancel", "approval-persist", "approval-invalid", "skills", "goal-rubric", "goal-startup", "approval-widget", "plugins", "workflow-fake"]) {
    const stateDirectory = path.join(temporary, `state-${name}`);
    await mkdir(stateDirectory, { recursive: true });
    await writeFile(
      path.join(stateDirectory, "approval.json"),
      '{"acknowledged":true,"auto_notice_shown":true,"auto_notice_version":"2026-07-24","policy_version":"2026-07-14","version":1}\n',
      { mode: 0o600 }
    );
  }

  const approvalThreadKey = createHash("sha256").update("playwright-newer").digest("hex");
  const approvalBase = {
    acknowledged: true,
    auto_notice_shown: true,
    auto_notice_version: "2026-07-24",
    policy_version: "2026-07-14",
    version: 1
  };
  await writeFile(
    path.join(temporary, "state-approval-persist", "approval.json"),
    `${JSON.stringify({ ...approvalBase, thread_approval_modes: { [approvalThreadKey]: { mode: "yolo" } } })}\n`,
    { mode: 0o600 }
  );
  await writeFile(
    path.join(temporary, "state-approval-invalid", "approval.json"),
    `${JSON.stringify({ ...approvalBase, thread_approval_modes: { [approvalThreadKey]: { mode: "unrestricted" } } })}\n`,
    { mode: 0o600 }
  );

  const startupWorkspace = path.join(temporary, "workspace-startup");
  await mkdir(startupWorkspace, { recursive: true });
  await writeFile(
    path.join(startupWorkspace, "startup.SKILL.md"),
    "---\nname: playwright-startup\ndescription: Verify startup automation\n---\nPlaywright startup skill instructions.\n",
    { mode: 0o600 }
  );
  await writeFile(path.join(startupWorkspace, "startup-output.txt"), "startup command output\n", { mode: 0o600 });
  const startupCommand =
    "mkdir -p .deepagents/skills/playwright-startup; " +
    "cp startup.SKILL.md .deepagents/skills/playwright-startup/SKILL.md; " +
    "cat startup-output.txt";

  const inputWorkspace = path.join(temporary, "workspace-input");
  await mkdir(inputWorkspace, { recursive: true });
  await writeFile(path.join(inputWorkspace, "mention-target.txt"), "mention fixture\n", { mode: 0o600 });
  await writeFile(path.join(inputWorkspace, "screen.png"), Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=", "base64"), { mode: 0o600 });

  const goalRubricWorkspace = path.join(temporary, "workspace-goal-rubric");
  await mkdir(goalRubricWorkspace, { recursive: true });
  await writeFile(
    path.join(goalRubricWorkspace, "release-rubric.md"),
    "- Release checklist is complete.\n- Verification passes.\n",
    { mode: 0o600 }
  );

  const approvalWorkspace = path.join(temporary, "workspace-approval");
  await mkdir(approvalWorkspace, { recursive: true });
  await writeFile(path.join(approvalWorkspace, "preview.md"), "keep\nold value\ntail\n", { mode: 0o600 });

  const transcriptWorkspace = path.join(temporary, "workspace-transcript");
  await mkdir(transcriptWorkspace, { recursive: true });
	await writeFile(path.join(transcriptWorkspace, "parallel-fixture.txt"), "parallel fixture\n", { mode: 0o600 });

  const themeHome = path.join(temporary, "home-theme");
  await mkdir(path.join(themeHome, ".deepagents"), { recursive: true });
  await writeFile(
    path.join(themeHome, ".deepagents", "config.toml"),
    `[ui]
theme = "langchain"

[themes.browser-custom]
label = "Browser Custom"
dark = true
primary = "#00AAFF"
secondary = "#FF77CC"
`,
    { mode: 0o600 }
  );

  await mkdir(path.join(temporary, "home"), { recursive: true });

  for (const name of [
    "default", "manual", "yolo", "resume", "direct-resume", "resume-flow", "offload", "offload-error",
    "restart-success", "restart-error", "restart-unavailable", "approval-persist", "approval-invalid", "hostile",
    "transcript", "transcript-history", "editor", "effort", "auto-notice", "unsupported-effort", "startup", "input",
    "ask-user", "ask-user-cancel", "skills", "goal-rubric", "goal-startup", "approval-widget", "theme", "plugins",
    "diagnostics", "fanout", "auth-mcp-auto", "auth-error", "auth-cancel", "workflow-fake"
  ]) {
    const stateDirectory = path.join(temporary, `state-${name}`);
    await mkdir(stateDirectory, { recursive: true });
    await writeFile(path.join(stateDirectory, "onboarding_complete"), "1\n", { mode: 0o600 });
  }

	const skillsWorkspace = path.join(temporary, "workspace-skills");
	const externalSkill = path.join(temporary, "external-skills", "playwright-external");
	await mkdir(path.join(skillsWorkspace, ".agents", "skills"), { recursive: true });
	await mkdir(externalSkill, { recursive: true });
	await writeFile(
	  path.join(externalSkill, "SKILL.md"),
	  "---\nname: playwright-external\ndescription: Verify external skill trust\n---\n\nPlaywright external skill instructions.\n",
	  { mode: 0o600 }
	);
	await symlink(externalSkill, path.join(skillsWorkspace, ".agents", "skills", "playwright-external"), "dir");

  const pluginWorkspace = path.join(temporary, "workspace-plugins");
  const pluginCatalog = path.join(temporary, "plugin-catalog");
  const extraPluginCatalog = path.join(temporary, "plugin-catalog-extra");
  const pluginStore = path.join(temporary, "state-plugins", "plugins");
  const activePlugin = path.join(pluginCatalog, "plugins", "active");
  const availablePlugin = path.join(pluginCatalog, "plugins", "available");
  await mkdir(path.join(pluginCatalog, ".agents", "plugins"), { recursive: true });
  await mkdir(path.join(extraPluginCatalog, ".agents", "plugins"), { recursive: true });
  await mkdir(activePlugin, { recursive: true });
  await mkdir(availablePlugin, { recursive: true });
  await mkdir(pluginWorkspace, { recursive: true });
  await writeFile(
    path.join(activePlugin, "plugin.json"),
    `${JSON.stringify({
      name: "active",
      displayName: "Active Browser Plugin",
      version: "1",
      hooks: {
        UserPromptSubmit: [
          { hooks: [{ type: "command", command: "/bin/sleep 2", statusMessage: "Checking browser plugin" }] }
        ]
      }
    })}\n`,
    { mode: 0o600 }
  );
  await writeFile(
    path.join(availablePlugin, "plugin.json"),
    `${JSON.stringify({ name: "available", displayName: "Available Browser Plugin", version: "1" })}\n`,
    { mode: 0o600 }
  );
  await writeFile(
    path.join(pluginCatalog, ".agents", "plugins", "marketplace.json"),
    `${JSON.stringify({
      name: "browser",
      plugins: [
        { name: "active", displayName: "Active Browser Plugin", description: "Provides the browser hook fixture", source: "./plugins/active" },
        { name: "available", displayName: "Available Browser Plugin", description: "Exercises plugin installation", source: "./plugins/available" }
      ]
    })}\n`,
    { mode: 0o600 }
  );
  await writeFile(
    path.join(extraPluginCatalog, ".agents", "plugins", "marketplace.json"),
    `${JSON.stringify({ name: "extra-browser", plugins: [] })}\n`,
    { mode: 0o600 }
  );
  for (const arguments_ of [
    ["plugin", "marketplace", "add", pluginCatalog, "--store", pluginStore],
    ["plugin", "install", "active@browser", "--store", pluginStore]
  ]) {
    const seeded = spawnSync(binary, arguments_, { cwd: root, encoding: "utf8" });
    if (seeded.status !== 0) {
      await rm(temporary, { force: true, recursive: true });
      throw new Error(`failed to seed plugin fixture:\n${seeded.stderr}`);
    }
  }

  const restartSuccessAddress = await availableLoopbackAddress();
  const restartErrorAddress = await availableLoopbackAddress();
  const restartFailureMarker = path.join(temporary, "restart-error-started");
  const liveEnabled = process.env.DAGO_PLAYWRIGHT_OPENAI_LIVE === "1";
  let liveServer: ChildProcessByStdio<null, Readable, Readable> | null = null;
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
    liveServer = startServer(binary, temporary, "openai-live", "", ["--approve-for-me", "-M", model], {}, true);
  }
  const servers = [
    startServer(binary, temporary, "default", slowAPIURL),
    startServer(binary, temporary, "manual", slowAPIURL, ["--manual-review"]),
    startServer(binary, temporary, "yolo", slowAPIURL, ["--yolo"]),
    startServer(binary, temporary, "resume", slowAPIURL, ["resume"]),
    startServer(binary, temporary, "direct-resume", slowAPIURL, ["resume", "playwright-newer"]),
    startServer(binary, temporary, "resume-flow", slowAPIURL, ["resume", "playwright-lifecycle"]),
    startServer(binary, temporary, "offload", slowAPIURL, ["resume", "playwright-offload"]),
    startServer(binary, temporary, "offload-error", failingAPIURL, ["resume", "--max-retries=0", "playwright-offload"]),
    startServer(binary, temporary, "onboarding", slowAPIURL),
    startServer(binary, temporary, "restart-success", slowAPIURL, [
      "--local-dev-server", restartBinary,
      `--local-dev-arg=--address`, `--local-dev-arg=${restartSuccessAddress}`,
      "--local-dev-endpoint", `http://${restartSuccessAddress}`,
      "--local-dev-health-path", "/health"
    ]),
    startServer(binary, temporary, "restart-error", slowAPIURL, [
      "--local-dev-server", restartBinary,
      `--local-dev-arg=--address`, `--local-dev-arg=${restartErrorAddress}`,
      `--local-dev-arg=--fail-after`, `--local-dev-arg=${restartFailureMarker}`,
      "--local-dev-endpoint", `http://${restartErrorAddress}`,
      "--local-dev-health-path", "/health"
    ]),
    startServer(binary, temporary, "restart-unavailable", slowAPIURL),
    startServer(binary, temporary, "approval-persist", slowAPIURL, ["resume", "playwright-newer"]),
    startServer(binary, temporary, "approval-invalid", slowAPIURL, ["resume", "playwright-newer"]),
    startServer(binary, temporary, "hostile", slowAPIURL, ["resume", "playwright-hostile"]),
	startServer(binary, temporary, "transcript", slowAPIURL, ["--yolo", "--cwd", transcriptWorkspace]),
	startServer(binary, temporary, "transcript-history", slowAPIURL, ["resume", "playwright-virtualized"]),
    startServer(binary, temporary, "editor", slowAPIURL, [], { VISUAL: editorBinary }),
    startServer(binary, temporary, "effort", slowAPIURL),
    startServer(binary, temporary, "auto-notice", slowAPIURL),
    startServer(unsupportedEffortBinary, temporary, "unsupported-effort", slowAPIURL),
    startServer(binary, temporary, "startup", slowAPIURL, [
      "--cwd",
      startupWorkspace,
      "--startup-cmd",
      startupCommand,
      "--skill",
      "playwright-startup",
      "--message",
      "inspect startup automation"
    ]),
    startServer(binary, temporary, "input", slowAPIURL, ["--cwd", inputWorkspace]),
    startServer(binary, temporary, "ask-user", slowAPIURL, [], { VISUAL: editorBinary }),
	startServer(binary, temporary, "ask-user-cancel", slowAPIURL),
	startServer(binary, temporary, "skills", slowAPIURL, ["--cwd", skillsWorkspace]),
    startServer(binary, temporary, "goal-rubric", slowAPIURL, ["--cwd", goalRubricWorkspace], { VISUAL: editorBinary }),
    startServer(binary, temporary, "goal-startup", slowAPIURL, ["--cwd", goalRubricWorkspace, "--goal", "Finish the release checklist"]),
    startServer(binary, temporary, "approval-widget", slowAPIURL, ["--manual-review", "--cwd", approvalWorkspace], { TAVILY_API_KEY: "playwright-placeholder" }),
    startServer(binary, temporary, "theme", slowAPIURL, ["--manual-review"], { HOME: themeHome, TERM_PROGRAM: "ThemeBrowser" }),
    startServer(binary, temporary, "plugins", slowAPIURL, ["--cwd", pluginWorkspace]),
    startServer(binary, temporary, "diagnostics", slowAPIURL, ["--manual-review"]),
    startServer(binary, temporary, "fanout", slowAPIURL, ["--manual-review"]),
    startServer(authMCPAutoBinary, temporary, "auth-mcp-auto", slowAPIURL),
    startServer(authMCPAutoBinary, temporary, "auth-error", slowAPIURL, [], { DACODE_FIXTURE_AUTH_MODE: "error" }),
    startServer(authMCPAutoBinary, temporary, "auth-cancel", slowAPIURL, [], { DACODE_FIXTURE_AUTH_MODE: "cancel" }),
    startServer(notificationUpdateTraceBinary, temporary, "notify", slowAPIURL),
    startServer(notificationUpdateTraceBinary, temporary, "notify-settings", slowAPIURL),
    startServer(notificationUpdateTraceBinary, temporary, "notify-actions", slowAPIURL),
    startServer(notificationUpdateTraceBinary, temporary, "notify-layout", slowAPIURL, [], { DACODE_FIXTURE_ASCII: "1" }),
    startServer(notificationUpdateTraceBinary, temporary, "notify-fail", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "preference-fail" }),
    startServer(notificationUpdateTraceBinary, temporary, "trace", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "trace-ok" }),
    startServer(notificationUpdateTraceBinary, temporary, "trace-fail", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "trace-fail" }),
    startServer(notificationUpdateTraceBinary, temporary, "trace-unconfigured", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "trace-unconfigured" }),
    startServer(notificationUpdateTraceBinary, temporary, "trace-timeout", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "trace-timeout" }),
    startServer(notificationUpdateTraceBinary, temporary, "trace-unsafe", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "trace-unsafe" }),
    startServer(notificationUpdateTraceBinary, temporary, "update-windows", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "update-windows" }),
    startServer(notificationUpdateTraceBinary, temporary, "update-current", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "update-current", DEEPAGENTS_CODE_AUTO_UPDATE: "0" }),
    startServer(notificationUpdateTraceBinary, temporary, "update-available", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "update-available" }),
    startServer(notificationUpdateTraceBinary, temporary, "update-available-ui", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "update-available", DEEPAGENTS_CODE_AUTO_UPDATE: "0" }),
    startServer(notificationUpdateTraceBinary, temporary, "update-startup-choice", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "update-available" }),
    startServer(notificationUpdateTraceBinary, temporary, "update-slow", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "update-slow-apply", DEEPAGENTS_CODE_AUTO_UPDATE: "0" }),
    startServer(notificationUpdateTraceBinary, temporary, "update-retry", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "update-retry", DEEPAGENTS_CODE_AUTO_UPDATE: "0" }),
    startServer(notificationUpdateTraceBinary, temporary, "update-fail", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "update-apply-fail" }),
    startServer(notificationUpdateTraceBinary, temporary, "update-shared", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "update-shared-lock", DEEPAGENTS_CODE_AUTO_UPDATE: "0" }),
    startServer(notificationUpdateTraceBinary, temporary, "auto-disabled", slowAPIURL, [], { DEEPAGENTS_CODE_AUTO_UPDATE: "0" }),
    startServer(notificationUpdateTraceBinary, temporary, "auto-malformed", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "auto-malformed" }),
    startServer(notificationUpdateTraceBinary, temporary, "auto-symlink", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "auto-symlink" }),
    startServer(notificationUpdateTraceBinary, temporary, "auto-unwritable", slowAPIURL, [], { DACODE_FIXTURE_NOTIFY_MODE: "auto-unwritable" }),
    startServer(interactionBinary, temporary, "interactions", slowAPIURL),
    startServer(interactionBinary, temporary, "interactions-direct", slowAPIURL, ["resume", "thread-alpha"]),
    startServer(interactionBinary, temporary, "interactions-startup-failed", slowAPIURL, [], { DACODE_FIXTURE_STARTUP_FAILED: "1" }),
    startServer(polishBinary, temporary, "polish", slowAPIURL),
    startServer(polishBinary, temporary, "polish-failure", slowAPIURL, [], { DAGO_POLISH_MODE: "failure" }),
    startServer(polishBinary, temporary, "polish-resumed", slowAPIURL, [], { DAGO_POLISH_MODE: "resumed" }),
    startServer(polishBinary, temporary, "polish-fallback", slowAPIURL, [], { DAGO_POLISH_MODE: "fallback" }),
    startServer(polishBinary, temporary, "polish-prompt", slowAPIURL, ["-m", "fixture initial prompt"]),
    startServer(polishBinary, temporary, "polish-goal", slowAPIURL, ["--goal", "fixture initial goal"]),
    startServer(polishBinary, temporary, "polish-queued", slowAPIURL, [], { DAGO_POLISH_MODE: "queued" }),
    startServer(polishBinary, temporary, "polish-anchor", slowAPIURL, [], { DAGO_POLISH_MODE: "anchor" }),
    startServer(polishBinary, temporary, "polish-hook", slowAPIURL, [], { DAGO_POLISH_MODE: "hook" }),
    startServer(polishBinary, temporary, "polish-ascii", slowAPIURL, [], { DAGO_POLISH_MODE: "ascii" }),
    startServer(binary, temporary, "workflow-fake", workflowFixture.url, ["--approve-for-me"])
  ];

  try {
    const [baseURL, manualURL, yoloURL, resumeURL, directResumeURL, resumeFlowURL, offloadURL, offloadErrorURL, onboardingURL, restartSuccessURL, restartErrorURL, restartUnavailableURL, approvalPersistURL, approvalInvalidURL, hostileURL, transcriptURL, transcriptHistoryURL, editorURL, effortURL, autoNoticeURL, unsupportedEffortURL, startupURL, inputURL, askUserURL, askUserCancelURL, skillsURL, goalRubricURL, goalStartupURL, approvalWidgetURL, themeURL, pluginsURL, diagnosticsURL, fanoutURL, authMCPAutoURL, authErrorURL, authCancelURL, notifyURL, notifySettingsURL, notifyActionsURL, notifyLayoutURL, notifyFailURL, traceURL, traceFailURL, traceUnconfiguredURL, traceTimeoutURL, traceUnsafeURL, updateWindowsURL, updateCurrentURL, updateAvailableURL, updateAvailableUIURL, updateStartupChoiceURL, updateSlowURL, updateRetryURL, updateFailURL, updateSharedURL, autoDisabledURL, autoMalformedURL, autoSymlinkURL, autoUnwritableURL, interactionsURL, interactionsDirectURL, interactionsStartupFailedURL, polishURL, polishFailureURL, polishResumedURL, polishFallbackURL, polishPromptURL, polishGoalURL, polishQueuedURL, polishAnchorURL, polishHookURL, polishASCIIURL, workflowFakeURL] = await Promise.all(
      servers.map(serverURL)
    );
    process.env.PLAYWRIGHT_TEST_BASE_URL = baseURL;
    process.env.PLAYWRIGHT_MANUAL_URL = manualURL;
    process.env.PLAYWRIGHT_YOLO_URL = yoloURL;
    process.env.PLAYWRIGHT_RESUME_URL = resumeURL;
    process.env.PLAYWRIGHT_DIRECT_RESUME_URL = directResumeURL;
    process.env.PLAYWRIGHT_RESUME_FLOW_URL = resumeFlowURL;
    process.env.PLAYWRIGHT_OFFLOAD_URL = offloadURL;
    process.env.PLAYWRIGHT_OFFLOAD_ERROR_URL = offloadErrorURL;
    process.env.PLAYWRIGHT_ONBOARDING_URL = onboardingURL;
    process.env.PLAYWRIGHT_RESTART_SUCCESS_URL = restartSuccessURL;
    process.env.PLAYWRIGHT_RESTART_ERROR_URL = restartErrorURL;
    process.env.PLAYWRIGHT_RESTART_UNAVAILABLE_URL = restartUnavailableURL;
    process.env.PLAYWRIGHT_APPROVAL_PERSIST_URL = approvalPersistURL;
    process.env.PLAYWRIGHT_APPROVAL_INVALID_URL = approvalInvalidURL;
    process.env.PLAYWRIGHT_HOSTILE_URL = hostileURL;
	process.env.PLAYWRIGHT_TRANSCRIPT_URL = transcriptURL;
	process.env.PLAYWRIGHT_TRANSCRIPT_HISTORY_URL = transcriptHistoryURL;
    process.env.PLAYWRIGHT_EDITOR_URL = editorURL;
    process.env.PLAYWRIGHT_EFFORT_URL = effortURL;
    process.env.PLAYWRIGHT_AUTO_NOTICE_URL = autoNoticeURL;
    process.env.PLAYWRIGHT_UNSUPPORTED_EFFORT_URL = unsupportedEffortURL;
    process.env.PLAYWRIGHT_STARTUP_URL = startupURL;
    process.env.PLAYWRIGHT_INPUT_URL = inputURL;
    process.env.PLAYWRIGHT_ASK_USER_URL = askUserURL;
    process.env.PLAYWRIGHT_ASK_USER_CANCEL_URL = askUserCancelURL;
	process.env.PLAYWRIGHT_SKILLS_URL = skillsURL;
    process.env.PLAYWRIGHT_GOAL_RUBRIC_URL = goalRubricURL;
    process.env.PLAYWRIGHT_GOAL_STARTUP_URL = goalStartupURL;
    process.env.PLAYWRIGHT_APPROVAL_WIDGET_URL = approvalWidgetURL;
    process.env.PLAYWRIGHT_THEME_URL = themeURL;
    process.env.PLAYWRIGHT_PLUGINS_URL = pluginsURL;
    process.env.PLAYWRIGHT_DIAGNOSTICS_URL = diagnosticsURL;
    process.env.PLAYWRIGHT_FANOUT_URL = fanoutURL;
    process.env.PLAYWRIGHT_AUTH_MCP_AUTO_URL = authMCPAutoURL;
    process.env.PLAYWRIGHT_AUTH_ERROR_URL = authErrorURL;
    process.env.PLAYWRIGHT_AUTH_CANCEL_URL = authCancelURL;
    process.env.PLAYWRIGHT_NOTIFY_URL = notifyURL;
    process.env.PLAYWRIGHT_NOTIFY_SETTINGS_URL = notifySettingsURL;
    process.env.PLAYWRIGHT_NOTIFY_ACTIONS_URL = notifyActionsURL;
    process.env.PLAYWRIGHT_NOTIFY_LAYOUT_URL = notifyLayoutURL;
    process.env.PLAYWRIGHT_NOTIFY_FAIL_URL = notifyFailURL;
    process.env.PLAYWRIGHT_TRACE_URL = traceURL;
    process.env.PLAYWRIGHT_TRACE_FAIL_URL = traceFailURL;
    process.env.PLAYWRIGHT_TRACE_UNCONFIGURED_URL = traceUnconfiguredURL;
    process.env.PLAYWRIGHT_TRACE_TIMEOUT_URL = traceTimeoutURL;
    process.env.PLAYWRIGHT_TRACE_UNSAFE_URL = traceUnsafeURL;
    process.env.PLAYWRIGHT_UPDATE_WINDOWS_URL = updateWindowsURL;
    process.env.PLAYWRIGHT_UPDATE_CURRENT_URL = updateCurrentURL;
    process.env.PLAYWRIGHT_UPDATE_AVAILABLE_URL = updateAvailableURL;
    process.env.PLAYWRIGHT_UPDATE_AVAILABLE_UI_URL = updateAvailableUIURL;
    process.env.PLAYWRIGHT_UPDATE_STARTUP_CHOICE_URL = updateStartupChoiceURL;
    process.env.PLAYWRIGHT_UPDATE_SLOW_URL = updateSlowURL;
    process.env.PLAYWRIGHT_UPDATE_RETRY_URL = updateRetryURL;
    process.env.PLAYWRIGHT_UPDATE_FAIL_URL = updateFailURL;
    process.env.PLAYWRIGHT_UPDATE_SHARED_URL = updateSharedURL;
    process.env.PLAYWRIGHT_AUTO_DISABLED_URL = autoDisabledURL;
    process.env.PLAYWRIGHT_AUTO_MALFORMED_URL = autoMalformedURL;
    process.env.PLAYWRIGHT_AUTO_SYMLINK_URL = autoSymlinkURL;
    process.env.PLAYWRIGHT_AUTO_UNWRITABLE_URL = autoUnwritableURL;
    process.env.PLAYWRIGHT_INTERACTIONS_URL = interactionsURL;
    process.env.PLAYWRIGHT_INTERACTIONS_DIRECT_URL = interactionsDirectURL;
    process.env.PLAYWRIGHT_INTERACTIONS_STARTUP_FAILED_URL = interactionsStartupFailedURL;
    process.env.PLAYWRIGHT_POLISH_URL = polishURL;
    process.env.PLAYWRIGHT_POLISH_FAILURE_URL = polishFailureURL;
    process.env.PLAYWRIGHT_POLISH_RESUMED_URL = polishResumedURL;
    process.env.PLAYWRIGHT_POLISH_FALLBACK_URL = polishFallbackURL;
    process.env.PLAYWRIGHT_POLISH_PROMPT_URL = polishPromptURL;
    process.env.PLAYWRIGHT_POLISH_GOAL_URL = polishGoalURL;
    process.env.PLAYWRIGHT_POLISH_QUEUED_URL = polishQueuedURL;
    process.env.PLAYWRIGHT_POLISH_ANCHOR_URL = polishAnchorURL;
    process.env.PLAYWRIGHT_POLISH_HOOK_URL = polishHookURL;
    process.env.PLAYWRIGHT_POLISH_ASCII_URL = polishASCIIURL;
    process.env.PLAYWRIGHT_WORKFLOW_FAKE_URL = workflowFakeURL;
    process.env.PLAYWRIGHT_WORKFLOW_FAKE_API_URL = workflowFixture.url;
    if (liveServer) process.env.PLAYWRIGHT_OPENAI_LIVE_URL = await serverURL(liveServer);
    process.env.PLAYWRIGHT_PLUGIN_EXTRA_CATALOG = extraPluginCatalog;
  } catch (error) {
    for (const server of servers) server.kill("SIGTERM");
    liveServer?.kill("SIGTERM");
    failingAPI.closeAllConnections();
    failingAPI.close();
    slowAPI.closeAllConnections();
    slowAPI.close();
    workflowFixture.server.closeAllConnections();
    workflowFixture.server.close();
    await rm(temporary, { force: true, recursive: true });
    throw error;
  }

  return async () => {
    await Promise.all(servers.map(stopServer));
    if (liveServer) await stopServer(liveServer);
    failingAPI.closeAllConnections();
    await new Promise<void>((resolve) => failingAPI.close(() => resolve()));
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
		localPathExecutions: 0,
		localPathPrompts: 0,
    recoveryAlternativeCalls: 0,
    recoveryContinuations: 0,
    scoutCalls: 0,
		structuredCorrections: 0,
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
		const sendStructured = (value: Record<string, unknown>) => {
			const structuredTool = Array.isArray(body.tools)
				? body.tools.find((tool: Record<string, unknown>) =>
					tool.name === "workflow_result" || tool.name === "approval_assessment")
				: undefined;
			if (structuredTool) {
				sendTool(`structured-result-${sequence}`, String(structuredTool.name), value);
				return;
			}
			sendText(JSON.stringify(value));
		};

    const hasApprovalTool = Array.isArray(body.tools)
      && body.tools.some((tool: Record<string, unknown>) => tool.name === "approval_assessment");
    if (formatName === "approval_assessment" || hasApprovalTool) {
      state.approvalReviews += 1;
      const denied = requestText.includes("DENY_BACKGROUND_ACTION");
      if (denied) state.deniedApprovalReviews += 1;
      if (requestText.includes("AUTO_REVIEW_VISIBILITY")) {
        await new Promise((resolve) => setTimeout(resolve, 600));
      }
      const toolCallID = requestText.match(/\\"tool_call_id\\"\s*:\s*\\"([^"\\]+)\\"/)?.[1]
        ?? requestText.match(/"tool_call_id"\s*:\s*"([^"]+)"/)?.[1]
        ?? "missing";
      sendStructured({ decisions: [{
        tool_call_id: toolCallID,
        risk_level: denied ? "high" : "low",
        user_authorization: denied ? "low" : "high",
        outcome: denied ? "deny" : "allow",
        rationale: denied
          ? "The requested action is outside the read-only workflow scope; use a safe inspection instead."
          : "The read-only repository inspection is within the requested workflow scope."
      }] });
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
          sendTool(`recovery-read-${sequence}`, "read_file", { file_path: path.join(root, "docs/ARCHITECTURE.md"), offset: 0, limit: 80 });
          return;
        }
        state.recoveryContinuations += 1;
		sendStructured({ findings: [{
          title: "Keep denied workflow workers alive for safe alternatives",
          files: ["internal/dacode/workflows.go"],
          problem: "Approval denial must be returned to the worker instead of terminating its branch.",
          refactor: "Preserve the approval resume loop and make terminal failure an explicit worker choice.",
          impact: "high",
          effort: "small",
          evidence: "The worker recovered from a denied execute call by reading docs/ARCHITECTURE.md."
		}] });
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
			if (!input.includes("workflow_result")) state.workerExecuteContinuations += 1;
      if (requestText.includes("SCAN_CORE")) {
			if (!input.includes("workflow_result") && state.structuredCorrections === 0) {
				state.structuredCorrections += 1;
				sendStructured({ findings: "invalid-first-result" });
				return;
			}
		sendStructured({ findings: [{
          title: "Consolidate workflow lifecycle transitions",
          files: ["daworkflow/manager.go", "daworkflow/runtime.go"],
          problem: "Lifecycle state and terminal failure accounting span the manager and runtime.",
          refactor: "Introduce one internal transition helper with explicit invariants.",
          impact: "high",
          effort: "medium",
          evidence: "daworkflow/manager.go owns terminal state while daworkflow/runtime.go emits agent_failed events."
		}] });
        return;
      }
      if (requestText.includes("SCAN_TUI")) {
		sendStructured({ findings: [{
          title: "Separate workflow panel projection from rendering",
          files: ["internal/dacode/workflow_tui.go"],
          problem: "Event aggregation and terminal layout are coupled in one panel implementation.",
          refactor: "Extract a tested run-view projection consumed by the renderer.",
          impact: "medium",
          effort: "small",
          evidence: "internal/dacode/workflow_tui.go computes event counts beside terminal presentation code."
		}] });
        return;
      }
      if (requestText.includes("VERIFY_FINDING")) {
		sendStructured({
          verdict: "confirmed",
          reasoning: "The cited files contain the stated cross-layer responsibility and the proposed boundary preserves behavior.",
          impact: "high",
          effort: "medium",
          sketch: "Add an internal projection or transition type, migrate callers, then retain contract tests."
		});
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

		if (requestText.includes("LOCAL_PATH_SEMANTICS")) {
			if (input.includes("function_call_output")) {
				state.localPathExecutions += 1;
				sendText("Local working-directory paths verified.");
				return;
			}
			if (instructions.includes(`current working directory is ${JSON.stringify(root)}`)) {
				state.localPathPrompts += 1;
			}
			sendTool("local-path-execute", "execute", { command: "pwd && find . -maxdepth 1 -name README.md -print" });
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
    sendTool("scout-read", "read_file", { file_path: path.join(root, "docs/ARCHITECTURE.md"), offset: 0, limit: 120 });
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
  extraEnvironment: Record<string, string> = {},
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
      "--model",
      "openai:gpt-5.6-terra",
      "--cwd",
      root,
      ...options
    ],
    {
      cwd: root,
      env: live
        ? { ...process.env, OPENAI_BASE_URL: "", TERM: "xterm-256color", ...extraEnvironment }
        : {
            ...process.env,
            HOME: path.join(temporary, "home"),
            OPENAI_API_KEY: "playwright-placeholder",
            OPENAI_BASE_URL: baseURL,
            TERM: "xterm-256color",
            ...extraEnvironment
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
