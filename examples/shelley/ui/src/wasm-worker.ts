/// <reference lib="webworker" />

import { Bash } from "just-bash/browser";

type WorkerRequest = {
  type: "request";
  id: number;
  method: string;
  url: string;
  headers: Record<string, string>;
  body: string;
};

type WorkerResponse = {
  status: number;
  headers?: Record<string, string>;
  body?: unknown;
  changed?: boolean;
  continue_conversation?: string;
};

type WasmWorkerGlobal = DedicatedWorkerGlobalScope & {
  Go: new () => {
    importObject: WebAssembly.Imports;
    run(instance: WebAssembly.Instance): Promise<void>;
  };
  shelleyWasmReady?: () => void;
  shelleyWasmRequest?: (request: string) => Promise<string>;
  shelleyWasmSnapshot?: () => string;
  shelleyWasmRestore?: (snapshot: string) => string;
  shelleyWasmContinue?: (conversationID: string) => boolean;
  shelleyWasmSetEventSink?: (sink: (event: string) => void) => void;
  shelleyJustBashExecute?: (request: string) => Promise<string>;
};

type ShellFile = {
  content: string;
  encoding: "utf-8" | "base64";
};

type ShellRequest = {
  command: string;
  cwd: string;
  timeout_milliseconds: number;
  files: Record<string, ShellFile>;
};

type ShellResponse = {
  stdout: string;
  stderr: string;
  exit_code: number;
  files: Record<string, ShellFile>;
};

const worker = self as unknown as WasmWorkerGlobal;
const databaseName = "shelley-wasm";
const snapshotKey = "application-state";
let persistence = Promise.resolve();
let activeEventBuffer: string[] | null = null;
let requests = Promise.resolve();

function decodeBase64(value: string): Uint8Array {
  const decoded = atob(value);
  const bytes = new Uint8Array(decoded.length);
  for (let index = 0; index < decoded.length; index++) bytes[index] = decoded.charCodeAt(index);
  return bytes;
}

function encodeBase64(value: Uint8Array): string {
  let encoded = "";
  for (let index = 0; index < value.length; index += 0x8000) {
    encoded += String.fromCharCode(...value.subarray(index, index + 0x8000));
  }
  return btoa(encoded);
}

async function executeJustBash(encoded: string): Promise<string> {
  const request = JSON.parse(encoded) as ShellRequest;
  const files: Record<string, string | Uint8Array> = {};
  for (const [path, file] of Object.entries(request.files || {})) {
    files[path] = file.encoding === "base64" ? decodeBase64(file.content) : file.content;
  }
  const bash = new Bash({ files, cwd: request.cwd || "/workspace" });
  const abort = new AbortController();
  const timer =
    request.timeout_milliseconds > 0
      ? setTimeout(() => abort.abort(), request.timeout_milliseconds)
      : undefined;
  let stdout = "";
  let stderr = "";
  let exitCode = 0;
  try {
    const result = await bash.exec(request.command, {
      cwd: request.cwd || "/workspace",
      signal: abort.signal,
    });
    stdout = result.stdout;
    stderr = result.stderr;
    exitCode = result.exitCode;
  } catch (error) {
    if (!abort.signal.aborted) throw error;
    stderr = `Command timed out after ${request.timeout_milliseconds}ms`;
    exitCode = 124;
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }

  const workspaceFiles: Record<string, ShellFile> = {};
  for (const path of bash.fs.getAllPaths()) {
    if (!path.startsWith("/workspace/")) continue;
    const stat = await bash.fs.stat(path);
    if (!stat.isFile) continue;
    const content = await bash.fs.readFileBuffer(path);
    try {
      workspaceFiles[path] = {
        content: new TextDecoder("utf-8", { fatal: true }).decode(content),
        encoding: "utf-8",
      };
    } catch {
      workspaceFiles[path] = { content: encodeBase64(content), encoding: "base64" };
    }
  }
  return JSON.stringify({
    stdout,
    stderr,
    exit_code: exitCode,
    files: workspaceFiles,
  } satisfies ShellResponse);
}

function openDatabase(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(databaseName, 1);
    request.onupgradeneeded = () => request.result.createObjectStore("state");
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error("failed to open browser database"));
  });
}

async function readSnapshot(): Promise<string> {
  const database = await openDatabase();
  try {
    return await new Promise((resolve, reject) => {
      const request = database
        .transaction("state", "readonly")
        .objectStore("state")
        .get(snapshotKey);
      request.onsuccess = () => resolve(typeof request.result === "string" ? request.result : "");
      request.onerror = () => reject(request.error ?? new Error("failed to read browser state"));
    });
  } finally {
    database.close();
  }
}

async function writeSnapshot(snapshot: string): Promise<void> {
  const database = await openDatabase();
  try {
    await new Promise<void>((resolve, reject) => {
      const transaction = database.transaction("state", "readwrite");
      transaction.objectStore("state").put(snapshot, snapshotKey);
      transaction.oncomplete = () => resolve();
      transaction.onerror = () =>
        reject(transaction.error ?? new Error("failed to persist browser state"));
      transaction.onabort = () =>
        reject(transaction.error ?? new Error("browser state transaction aborted"));
    });
  } finally {
    database.close();
  }
}

function schedulePersistence(): Promise<void> {
  const operation = persistence.then(async () => {
    const snapshot = worker.shelleyWasmSnapshot?.();
    if (typeof snapshot !== "string") throw new Error("WASM snapshot function is unavailable");
    await writeSnapshot(snapshot);
  });
  persistence = operation.catch((error) => {
    worker.postMessage({ type: "runtime-error", error: String(error) });
  });
  return operation;
}

function persistAndPublishEvent(event: string): Promise<void> {
  return schedulePersistence().then(
    () => worker.postMessage({ type: "stream-event", event }),
    () => undefined,
  );
}

async function initialize(): Promise<void> {
  worker.shelleyJustBashExecute = executeJustBash;
  const asset = (name: string) => new URL(name, worker.location.href).toString();
  importScripts(asset("wasm_exec.js"));
  if (typeof worker.Go !== "function") throw new Error("Go WASM support script did not initialize");

  const ready = new Promise<void>((resolve) => {
    worker.shelleyWasmReady = resolve;
  });
  const go = new worker.Go();
  const result = await WebAssembly.instantiateStreaming(
    fetch(asset("shelley.wasm")),
    go.importObject,
  );
  void go.run(result.instance);
  await ready;

  const restore = worker.shelleyWasmRestore;
  if (!restore) throw new Error("WASM restore function is unavailable");
  const restoreError = restore(await readSnapshot());
  if (restoreError) throw new Error(restoreError);

  const setEventSink = worker.shelleyWasmSetEventSink;
  if (!setEventSink) throw new Error("WASM event function is unavailable");
  setEventSink((event) => {
    if (activeEventBuffer) {
      activeEventBuffer.push(event);
      return;
    }
    void persistAndPublishEvent(event);
  });

  worker.addEventListener("message", (message: MessageEvent<WorkerRequest>) => {
    if (message.data.type !== "request") return;
    requests = requests.then(() => handleRequest(message.data));
  });
  worker.postMessage({ type: "ready" });
}

async function handleRequest(request: WorkerRequest): Promise<void> {
  activeEventBuffer = [];
  try {
    const invoke = worker.shelleyWasmRequest;
    if (!invoke) throw new Error("WASM request function is unavailable");
    const response = JSON.parse(await invoke(JSON.stringify(request))) as WorkerResponse;
    const buffered = activeEventBuffer ?? [];
    if (response.changed) {
      const responsePersistence = schedulePersistence();
      const eventDeliveries = buffered.map((event) => persistAndPublishEvent(event));
      activeEventBuffer = null;
      await responsePersistence;
      worker.postMessage({ type: "response", id: request.id, response });
      void Promise.all(eventDeliveries);
      continueConversation(response);
    } else {
      activeEventBuffer = null;
      worker.postMessage({ type: "response", id: request.id, response });
      for (const event of buffered) void persistAndPublishEvent(event);
      continueConversation(response);
    }
  } catch (error) {
    activeEventBuffer = null;
    worker.postMessage({ type: "response-error", id: request.id, error: String(error) });
  }
}

function continueConversation(response: WorkerResponse): void {
  if (!response.continue_conversation) return;
  const proceed = worker.shelleyWasmContinue;
  if (!proceed || !proceed(response.continue_conversation)) {
    worker.postMessage({
      type: "runtime-error",
      error: `failed to continue conversation ${response.continue_conversation}`,
    });
  }
}

initialize().catch((error) => worker.postMessage({ type: "runtime-error", error: String(error) }));
