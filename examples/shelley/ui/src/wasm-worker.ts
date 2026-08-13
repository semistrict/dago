/// <reference lib="webworker" />

import { WasmFileSystemAdapter } from "@semistrict/dawasm-browser/filesystem";
import { createIndexedDBCheckpointStore } from "@semistrict/dawasm-browser/checkpoint";
import { createBrowserFileStore } from "@semistrict/dawasm-browser/indexeddb";
import { JustBashRuntime } from "@semistrict/dawasm-browser/just-bash";
import {
  interruptWebGPUModel,
  invokeWebGPUModel,
  loadWebGPUModel,
} from "@semistrict/dawasm-browser/webgpu-qwen";

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

type UploadRequest = {
  type: "upload";
  id: number;
  name: string;
  bytes: ArrayBuffer;
};

type IncomingMessage =
  | WorkerRequest
  | ConfigureWebGPURequest
  | ConnectLocalDirectoryRequest
  | DisconnectLocalDirectoryRequest
  | UploadRequest;

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
  shelleyWasmFilesystem?: (operation: string, payload: string) => Promise<string>;
  shelleyWasmFilesystemPaths?: () => string;
  shelleyWasmConnectDirectory?: (handle: FileSystemDirectoryHandle) => Promise<string>;
  shelleyWasmDisconnectDirectory?: () => string;
  shelleyBrowserFileStore?: (operation: string, payload: string) => Promise<string>;
  shelleyJustBashExecute?: (request: string) => Promise<string>;
  shelleyWebGPUInvoke?: (request: string) => Promise<string>;
  shelleyWebGPUInterrupt?: () => Promise<void>;
  shelleyCheckpointStore?: (operation: string, payload: string) => Promise<string>;
};

const worker = self as unknown as WasmWorkerGlobal;
const databaseName = "shelley-wasm-runtime";
const databaseVersion = 1;
const conversationStore = "conversations";
const fileStore = "files";
const checkpointStoreName = "checkpoints";
const checkpointWriteStore = "checkpoint_writes";
const fileMetadataPrefix = "::metadata::";
let persistence = Promise.resolve();
let activeEventBuffer: string[] | null = null;
let requests = Promise.resolve();
const browserFilesystem = new WasmFileSystemAdapter({
  execute: async (operation, payload) => {
    const execute = worker.shelleyWasmFilesystem;
    if (!execute) throw new Error("Go browser filesystem is unavailable");
    return execute(operation, payload);
  },
  paths: () => worker.shelleyWasmFilesystemPaths?.() ?? "[]",
});
let browserShell: JustBashRuntime | null = null;
let storedConversations = new Map<string, string>();

type ApplicationSnapshot = {
  version: number;
  conversations: Record<string, unknown>;
};

async function executeJustBash(encoded: string): Promise<string> {
  if (!browserShell) throw new Error("browser shell is unavailable");
  return browserShell.executeJSON(encoded);
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
    const transaction = database.transaction(conversationStore, "readonly");
    const conversations = await requestResult<Array<{ id: string; value: unknown }>>(
      transaction.objectStore(conversationStore).getAll(),
    );
    storedConversations = new Map(
      conversations.map((record) => [record.id, JSON.stringify(record.value)]),
    );
    if (conversations.length === 0) return "";
    return JSON.stringify({
      version: 1,
      conversations: Object.fromEntries(conversations.map((record) => [record.id, record.value])),
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
  const database = await openDatabase();
  try {
    const transaction = database.transaction(conversationStore, "readwrite");
    const conversationRecords = transaction.objectStore(conversationStore);
    for (const [id, encoded] of conversations) {
      if (storedConversations.get(id) !== encoded) {
        conversationRecords.put({ id, value: JSON.parse(encoded) });
      }
    }
    for (const id of storedConversations.keys()) {
      if (!conversations.has(id)) conversationRecords.delete(id);
    }
    await transactionDone(transaction);
    storedConversations = conversations;
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
  try {
    const decoded = JSON.parse(event) as Record<string, unknown>;
    if (decoded.stream_delta || decoded.tool_progress) {
      worker.postMessage({ type: "stream-event", event });
      return Promise.resolve();
    }
  } catch {
    // Invalid events still go through persistence before delivery so a
    // transport bug cannot make durable state race ahead of the UI.
  }
  return schedulePersistence().then(
    () => worker.postMessage({ type: "stream-event", event }),
    () => undefined,
  );
}

async function initialize(): Promise<void> {
  browserShell = new JustBashRuntime({ filesystem: browserFilesystem });
  worker.shelleyJustBashExecute = executeJustBash;
  worker.shelleyBrowserFileStore = createBrowserFileStore({
    openDatabase,
    storeName: fileStore,
    metadataPrefix: fileMetadataPrefix,
  });
  worker.shelleyWebGPUInvoke = invokeWebGPUModel;
  worker.shelleyWebGPUInterrupt = interruptWebGPUModel;
  worker.shelleyCheckpointStore = createIndexedDBCheckpointStore({
    openDatabase,
    checkpointStore: checkpointStoreName,
    writeStore: checkpointWriteStore,
  });
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
      return;
    }
    if (message.data.type === "upload") {
      const request = message.data;
      requests = requests.then(() => uploadFile(request));
    }
  });
  worker.postMessage({ type: "ready" });
}

function safeUploadName(name: string): string {
  const basename = name.split(/[\\/]/).at(-1) || "upload";
  const cleaned = basename.replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^[-.]+|[-.]+$/g, "");
  return cleaned || "upload";
}

async function uploadFile(request: UploadRequest): Promise<void> {
  try {
    const path = `/workspace/uploads/${crypto.randomUUID()}-${safeUploadName(request.name)}`;
    await browserFilesystem.writeFile(
      path.slice("/workspace".length),
      new Uint8Array(request.bytes),
    );
    worker.postMessage({
      type: "response",
      id: request.id,
      response: {
        status: 201,
        headers: { "Content-Type": "application/json" },
        body: { path },
      },
    });
  } catch (error) {
    worker.postMessage({ type: "response-error", id: request.id, error: String(error) });
  }
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

async function connectLocalDirectory(id: number, handle: FileSystemDirectoryHandle): Promise<void> {
  try {
    worker.postMessage({ type: "local-directory-progress", id, text: "Indexing project paths…" });
    const connect = worker.shelleyWasmConnectDirectory;
    if (!connect) throw new Error("Go browser filesystem is unavailable");
    const info = JSON.parse(await connect(handle));
    worker.postMessage({ type: "local-directory-connected", id, info });
  } catch (error) {
    worker.shelleyWasmDisconnectDirectory?.();
    worker.postMessage({
      type: "local-directory-error",
      id,
      error: error instanceof Error ? error.message : String(error),
    });
  }
}

async function disconnectLocalDirectory(id: number): Promise<void> {
  worker.shelleyWasmDisconnectDirectory?.();
  worker.postMessage({ type: "local-directory-disconnected", id });
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
