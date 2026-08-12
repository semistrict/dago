/// <reference lib="webworker" />

import { Bash } from "just-bash/browser";
import { BrowserDirectoryWorkspace, type BrowserWorkspaceFile } from "./services/browserDirectory";
import { interruptWebGPUModel, invokeWebGPUModel, loadWebGPUModel } from "./services/webgpuModel";

type WorkerRequest = {
  type: "request";
  id: number;
  method: string;
  url: string;
  headers: Record<string, string>;
  body: string;
};

type ConfigureWebGPURequest = {
  type: "configure-webgpu-model";
  id: number;
};

type ConnectLocalDirectoryRequest = {
  type: "connect-local-directory";
  id: number;
  handle: FileSystemDirectoryHandle;
};

type DisconnectLocalDirectoryRequest = {
  type: "disconnect-local-directory";
  id: number;
};

type IncomingMessage =
  | WorkerRequest
  | ConfigureWebGPURequest
  | ConnectLocalDirectoryRequest
  | DisconnectLocalDirectoryRequest;

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
  shelleyWasmConfigureWebGPUModel?: () => string;
  shelleyWasmReplaceWorkspace?: (files: string) => string;
  shelleyWasmWorkspaceSnapshot?: () => string;
  shelleyJustBashExecute?: (request: string) => Promise<string>;
  shelleyWebGPUInvoke?: (request: string) => Promise<string>;
  shelleyWebGPUInterrupt?: () => Promise<void>;
  shelleyCheckpointStore?: (operation: string, payload: string) => Promise<string>;
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
const databaseName = "shelley-wasm-runtime";
const databaseVersion = 1;
const conversationStore = "conversations";
const fileStore = "files";
const checkpointStoreName = "checkpoints";
const checkpointWriteStore = "checkpoint_writes";
let persistence = Promise.resolve();
let activeEventBuffer: string[] | null = null;
let requests = Promise.resolve();
const localDirectory = new BrowserDirectoryWorkspace();
let storedConversations = new Map<string, string>();
let storedFiles = new Map<string, string>();

type ApplicationSnapshot = {
  version: number;
  conversations: Record<string, unknown>;
  files: Record<string, BrowserWorkspaceFile>;
};

type CheckpointRecord = {
  thread_id: string;
  namespace: string;
  checkpoint_id: string;
  parent_checkpoint_id?: string;
  type: string;
  checkpoint: string;
  metadata: unknown;
};

type CheckpointWriteRecord = {
  thread_id: string;
  namespace: string;
  checkpoint_id: string;
  task_id: string;
  task_path?: string;
  index: number;
  channel: string;
  type: string;
  value: string;
  replace?: boolean;
};

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
    const request = indexedDB.open(databaseName, databaseVersion);
    request.onupgradeneeded = () => {
      const database = request.result;
      if (!database.objectStoreNames.contains(conversationStore)) {
        database.createObjectStore(conversationStore, { keyPath: "id" });
      }
      if (!database.objectStoreNames.contains(fileStore)) {
        database.createObjectStore(fileStore, { keyPath: "path" });
      }
      if (!database.objectStoreNames.contains(checkpointStoreName)) {
        const checkpoints = database.createObjectStore(checkpointStoreName, {
          keyPath: ["thread_id", "namespace", "checkpoint_id"],
        });
        checkpoints.createIndex("by_thread", "thread_id");
        checkpoints.createIndex("by_thread_namespace", ["thread_id", "namespace"]);
      }
      if (!database.objectStoreNames.contains(checkpointWriteStore)) {
        const writes = database.createObjectStore(checkpointWriteStore, {
          keyPath: ["thread_id", "namespace", "checkpoint_id", "task_id", "index"],
        });
        writes.createIndex("by_thread", "thread_id");
        writes.createIndex("by_checkpoint", ["thread_id", "namespace", "checkpoint_id"]);
      }
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error("failed to open browser database"));
  });
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error("IndexedDB request failed"));
  });
}

function transactionDone(transaction: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    transaction.oncomplete = () => resolve();
    transaction.onerror = () =>
      reject(transaction.error ?? new Error("IndexedDB transaction failed"));
    transaction.onabort = () =>
      reject(transaction.error ?? new Error("IndexedDB transaction aborted"));
  });
}

async function readSnapshot(): Promise<string> {
  const database = await openDatabase();
  try {
    const transaction = database.transaction([conversationStore, fileStore], "readonly");
    const [conversations, files] = await Promise.all([
      requestResult<Array<{ id: string; value: unknown }>>(
        transaction.objectStore(conversationStore).getAll(),
      ),
      requestResult<Array<{ path: string; value: BrowserWorkspaceFile }>>(
        transaction.objectStore(fileStore).getAll(),
      ),
    ]);
    storedConversations = new Map(
      conversations.map((record) => [record.id, JSON.stringify(record.value)]),
    );
    storedFiles = new Map(files.map((record) => [record.path, JSON.stringify(record.value)]));
    if (conversations.length === 0 && files.length === 0) return "";
    return JSON.stringify({
      version: 1,
      conversations: Object.fromEntries(conversations.map((record) => [record.id, record.value])),
      files: Object.fromEntries(files.map((record) => [record.path, record.value])),
    } satisfies ApplicationSnapshot);
  } finally {
    database.close();
  }
}

async function writeSnapshot(snapshot: string): Promise<void> {
  const value = JSON.parse(snapshot) as ApplicationSnapshot;
  const conversations = new Map(
    Object.entries(value.conversations || {}).map(([id, record]) => [id, JSON.stringify(record)]),
  );
  const files = new Map(
    Object.entries(value.files || {}).map(([path, file]) => [path, JSON.stringify(file)]),
  );
  const database = await openDatabase();
  try {
    const transaction = database.transaction([conversationStore, fileStore], "readwrite");
    const conversationRecords = transaction.objectStore(conversationStore);
    const fileRecords = transaction.objectStore(fileStore);
    for (const [id, encoded] of conversations) {
      if (storedConversations.get(id) !== encoded) {
        conversationRecords.put({ id, value: JSON.parse(encoded) });
      }
    }
    for (const id of storedConversations.keys()) {
      if (!conversations.has(id)) conversationRecords.delete(id);
    }
    for (const [path, encoded] of files) {
      if (storedFiles.get(path) !== encoded) {
        fileRecords.put({ path, value: JSON.parse(encoded) });
      }
    }
    for (const path of storedFiles.keys()) {
      if (!files.has(path)) fileRecords.delete(path);
    }
    await transactionDone(transaction);
    storedConversations = conversations;
    storedFiles = files;
  } finally {
    database.close();
  }
}

async function checkpointRecordsForThread(
  database: IDBDatabase,
  storeName: string,
  threadID: string,
): Promise<Array<CheckpointRecord | CheckpointWriteRecord>> {
  const transaction = database.transaction(storeName, "readonly");
  return requestResult(
    transaction.objectStore(storeName).index("by_thread").getAll(IDBKeyRange.only(threadID)),
  ) as Promise<Array<CheckpointRecord | CheckpointWriteRecord>>;
}

function deleteIndexMatches(
  store: IDBObjectStore,
  indexName: string,
  query: IDBValidKey | IDBKeyRange,
): void {
  const request = store.index(indexName).openKeyCursor(query);
  request.onsuccess = () => {
    const cursor = request.result;
    if (!cursor) return;
    store.delete(cursor.primaryKey);
    cursor.continue();
  };
}

async function executeCheckpointStore(operation: string, encoded: string): Promise<string> {
  const payload = JSON.parse(encoded || "null") as Record<string, unknown> | null;
  const database = await openDatabase();
  try {
    switch (operation) {
      case "put_checkpoint": {
        const transaction = database.transaction(checkpointStoreName, "readwrite");
        transaction.objectStore(checkpointStoreName).put(payload as CheckpointRecord);
        await transactionDone(transaction);
        return "";
      }
      case "put_writes": {
        const records = (payload?.writes || []) as CheckpointWriteRecord[];
        const transaction = database.transaction(checkpointWriteStore, "readwrite");
        const store = transaction.objectStore(checkpointWriteStore);
        for (const record of records) {
          const stored = { ...record };
          delete stored.replace;
          if (record.replace) {
            store.put(stored);
            continue;
          }
          const key = [
            record.thread_id,
            record.namespace,
            record.checkpoint_id,
            record.task_id,
            record.index,
          ];
          const existing = store.get(key);
          existing.onsuccess = () => {
            if (existing.result === undefined) store.put(stored);
          };
        }
        await transactionDone(transaction);
        return "";
      }
      case "get_checkpoint": {
        const config = payload as {
          thread_id: string;
          checkpoint_ns?: string;
          checkpoint_id?: string;
        };
        const namespace = config.checkpoint_ns || "";
        const transaction = database.transaction(checkpointStoreName, "readonly");
        const store = transaction.objectStore(checkpointStoreName);
        if (config.checkpoint_id) {
          const record = await requestResult<CheckpointRecord | undefined>(
            store.get([config.thread_id, namespace, config.checkpoint_id]),
          );
          return JSON.stringify(record ?? null);
        }
        const records = await requestResult<CheckpointRecord[]>(
          store
            .index("by_thread_namespace")
            .getAll(IDBKeyRange.only([config.thread_id, namespace])),
        );
        const latest = records.reduce<CheckpointRecord | null>(
          (result, record) =>
            !result || record.checkpoint_id > result.checkpoint_id ? record : result,
          null,
        );
        return JSON.stringify(latest);
      }
      case "get_writes": {
        const config = payload as {
          thread_id: string;
          checkpoint_ns?: string;
          checkpoint_id: string;
        };
        const transaction = database.transaction(checkpointWriteStore, "readonly");
        const records = await requestResult<CheckpointWriteRecord[]>(
          transaction
            .objectStore(checkpointWriteStore)
            .index("by_checkpoint")
            .getAll(
              IDBKeyRange.only([
                config.thread_id,
                config.checkpoint_ns || "",
                config.checkpoint_id,
              ]),
            ),
        );
        return JSON.stringify(records);
      }
      case "list_checkpoints": {
        const config = payload as { thread_id?: string; checkpoint_ns?: string } | null;
        const transaction = database.transaction(checkpointStoreName, "readonly");
        const store = transaction.objectStore(checkpointStoreName);
        const records = config?.thread_id
          ? await requestResult<CheckpointRecord[]>(
              store.index("by_thread").getAll(IDBKeyRange.only(config.thread_id)),
            )
          : await requestResult<CheckpointRecord[]>(store.getAll());
        const namespace = config?.checkpoint_ns || "";
        return JSON.stringify(
          config ? records.filter((record) => record.namespace === namespace) : records,
        );
      }
      case "delete_thread": {
        const threadID = String(payload?.thread_id || "");
        const transaction = database.transaction(
          [checkpointStoreName, checkpointWriteStore],
          "readwrite",
        );
        deleteIndexMatches(
          transaction.objectStore(checkpointStoreName),
          "by_thread",
          IDBKeyRange.only(threadID),
        );
        deleteIndexMatches(
          transaction.objectStore(checkpointWriteStore),
          "by_thread",
          IDBKeyRange.only(threadID),
        );
        await transactionDone(transaction);
        return "";
      }
      case "copy_thread": {
        const source = String(payload?.source_thread_id || "");
        const target = String(payload?.target_thread_id || "");
        const [sourceCheckpoints, sourceWrites, targetCheckpoints] = await Promise.all([
          checkpointRecordsForThread(database, checkpointStoreName, source) as Promise<
            CheckpointRecord[]
          >,
          checkpointRecordsForThread(database, checkpointWriteStore, source) as Promise<
            CheckpointWriteRecord[]
          >,
          checkpointRecordsForThread(database, checkpointStoreName, target),
        ]);
        if (targetCheckpoints.length > 0) {
          throw new Error(`checkpoint target ${JSON.stringify(target)} already exists`);
        }
        const transaction = database.transaction(
          [checkpointStoreName, checkpointWriteStore],
          "readwrite",
        );
        const checkpoints = transaction.objectStore(checkpointStoreName);
        const writes = transaction.objectStore(checkpointWriteStore);
        for (const record of sourceCheckpoints) checkpoints.put({ ...record, thread_id: target });
        for (const record of sourceWrites) writes.put({ ...record, thread_id: target });
        await transactionDone(transaction);
        return "";
      }
      case "delete_checkpoints": {
        const configs = (payload?.configs || []) as Array<{
          thread_id: string;
          checkpoint_ns?: string;
          checkpoint_id: string;
        }>;
        const transaction = database.transaction(
          [checkpointStoreName, checkpointWriteStore],
          "readwrite",
        );
        const checkpoints = transaction.objectStore(checkpointStoreName);
        const writes = transaction.objectStore(checkpointWriteStore);
        for (const config of configs) {
          const key = [config.thread_id, config.checkpoint_ns || "", config.checkpoint_id];
          checkpoints.delete(key);
          deleteIndexMatches(writes, "by_checkpoint", IDBKeyRange.only(key));
        }
        await transactionDone(transaction);
        return "";
      }
      default:
        throw new Error(`unsupported checkpoint operation ${JSON.stringify(operation)}`);
    }
  } finally {
    database.close();
  }
}

function schedulePersistence(): Promise<void> {
  const operation = persistence.then(async () => {
    const snapshot = worker.shelleyWasmSnapshot?.();
    if (typeof snapshot !== "string") throw new Error("WASM snapshot function is unavailable");
    await syncLocalDirectory();
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
  worker.shelleyWebGPUInvoke = invokeWebGPUModel;
  worker.shelleyWebGPUInterrupt = interruptWebGPUModel;
  worker.shelleyCheckpointStore = executeCheckpointStore;
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

  worker.addEventListener("message", (message: MessageEvent<IncomingMessage>) => {
    if (message.data.type === "request") {
      const request = message.data;
      requests = requests.then(() => handleRequest(request));
      return;
    }
    if (message.data.type === "configure-webgpu-model") {
      requests = requests.then(() => configureWebGPUModel(message.data.id));
      return;
    }
    if (message.data.type === "connect-local-directory") {
      const request = message.data;
      requests = requests.then(() => connectLocalDirectory(request.id, request.handle));
      return;
    }
    if (message.data.type === "disconnect-local-directory") {
      requests = requests.then(() => disconnectLocalDirectory(message.data.id));
    }
  });
  worker.postMessage({ type: "ready" });
}

async function configureWebGPUModel(id: number): Promise<void> {
  try {
    await loadWebGPUModel((report) => {
      worker.postMessage({
        type: "webgpu-model-progress",
        progress: report.progress,
        text: report.text,
      });
    });
    const configure = worker.shelleyWasmConfigureWebGPUModel;
    if (!configure) throw new Error("WASM WebGPU model bridge is unavailable");
    const configureError = configure();
    if (configureError) throw new Error(configureError);
    worker.postMessage({ type: "webgpu-model-configured", id, model: "local-webgpu" });
  } catch (error) {
    worker.postMessage({
      type: "webgpu-model-error",
      id,
      error: error instanceof Error ? error.message : String(error),
    });
  }
}

function replaceWorkspace(files: Record<string, BrowserWorkspaceFile>): void {
  const replace = worker.shelleyWasmReplaceWorkspace;
  if (!replace) throw new Error("WASM workspace bridge is unavailable");
  const error = replace(JSON.stringify(files));
  if (error) throw new Error(error);
}

function workspaceSnapshot(): Record<string, BrowserWorkspaceFile> {
  const snapshot = worker.shelleyWasmWorkspaceSnapshot;
  if (!snapshot) throw new Error("WASM workspace snapshot is unavailable");
  return JSON.parse(snapshot()) as Record<string, BrowserWorkspaceFile>;
}

async function connectLocalDirectory(id: number, handle: FileSystemDirectoryHandle): Promise<void> {
  try {
    worker.postMessage({ type: "local-directory-progress", id, text: "Reading project files…" });
    const { files, info } = await localDirectory.connect(handle);
    replaceWorkspace(files);
    await schedulePersistence();
    worker.postMessage({ type: "local-directory-connected", id, info });
  } catch (error) {
    await localDirectory.disconnect();
    worker.postMessage({
      type: "local-directory-error",
      id,
      error: error instanceof Error ? error.message : String(error),
    });
  }
}

async function disconnectLocalDirectory(id: number): Promise<void> {
  await localDirectory.disconnect();
  worker.postMessage({ type: "local-directory-disconnected", id });
}

async function refreshLocalDirectory(): Promise<void> {
  try {
    const files = await localDirectory.refresh();
    if (files) replaceWorkspace(files);
  } catch (error) {
    await localDirectory.disconnect();
    worker.postMessage({
      type: "local-directory-sync-error",
      error: error instanceof Error ? error.message : String(error),
    });
  }
}

async function syncLocalDirectory(): Promise<void> {
  try {
    await localDirectory.sync(workspaceSnapshot());
  } catch (error) {
    await localDirectory.disconnect();
    worker.postMessage({
      type: "local-directory-sync-error",
      error: error instanceof Error ? error.message : String(error),
    });
  }
}

async function handleRequest(request: WorkerRequest): Promise<void> {
  activeEventBuffer = [];
  try {
    await refreshLocalDirectory();
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
