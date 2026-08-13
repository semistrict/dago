import { appPath } from "../basePath";
import { BrowserDirectoryHandleStore } from "@semistrict/dawasm-browser/directory-handle";
import type { BrowserDirectoryInfo } from "@semistrict/dawasm-browser/filesystem";

type PendingRequest = {
  resolve: (response: Response) => void;
  reject: (error: Error) => void;
};

type PendingWebGPUSetup = {
  resolve: (model: string) => void;
  reject: (error: Error) => void;
  onProgress?: (progress: number, text: string) => void;
};

type PendingLocalDirectory = {
  resolve: (info: BrowserDirectoryInfo) => void;
  reject: (error: Error) => void;
  onProgress?: (text: string) => void;
};

type WorkerResponse = {
  status: number;
  headers?: Record<string, string>;
  body?: unknown;
};

type RuntimeMessage =
  | { type: "ready" }
  | { type: "runtime-error"; error: string }
  | { type: "response"; id: number; response: WorkerResponse }
  | { type: "response-error"; id: number; error: string }
  | { type: "stream-event"; event: string }
  | { type: "webgpu-model-progress"; progress: number; text: string }
  | { type: "webgpu-model-configured"; id: number; model: string }
  | { type: "webgpu-model-error"; id: number; error: string }
  | { type: "local-directory-progress"; id: number; text: string }
  | { type: "local-directory-connected"; id: number; info: BrowserDirectoryInfo }
  | { type: "local-directory-disconnected"; id: number }
  | { type: "local-directory-error"; id: number; error: string }
  | { type: "local-directory-sync-error"; error: string };

const runtimePaths = ["/api/", "/feature-flags", "/version-check", "/upgrade", "/exit"];
const browserPredictableModelKey = "shelley_wasm_predictable_model";
const browserOpenAIKeyStorageKey = "shelley_wasm_openai_key";
const browserOpenAITestEndpointKey = "shelley_wasm_openai_test_endpoint";
const browserModelBackendKey = "shelley_wasm_model_backend";
const browserCustomModelsKey = "shelley_wasm_custom_models";
const browserDirectoryHandles = new BrowserDirectoryHandleStore({
  databaseName: "shelley-local-directory",
  storeName: "handles",
  handleKey: "workspace",
  pickerID: "shelley-workspace",
});
let browserOpenAIConfigured = false;
let browserWebGPUConfigured = false;
let rememberedDirectoryHandle: FileSystemDirectoryHandle | null = null;
let browserDirectoryReconnect = false;
let connectedDirectory: BrowserDirectoryInfo | null = null;

type StoredBrowserModel = Record<string, unknown> & { model_id: string; api_key: string };

function storedBrowserModels(): StoredBrowserModel[] {
  try {
    const value = JSON.parse(localStorage.getItem(browserCustomModelsKey) || "[]");
    return Array.isArray(value) ? (value as StoredBrowserModel[]) : [];
  } catch {
    return [];
  }
}

function saveBrowserModels(models: StoredBrowserModel[]): void {
  localStorage.setItem(browserCustomModelsKey, JSON.stringify(models));
}

async function rememberBrowserModel(
  request: Request,
  body: string,
  response: Response,
): Promise<void> {
  if (!response.ok) return;
  const path = new URL(request.url).pathname;
  if (!path.startsWith("/api/custom-models") || path === "/api/custom-models-test") return;
  const models = storedBrowserModels();
  const tail = path.slice("/api/custom-models".length).replace(/^\//, "");
  const [encodedID, action] = tail.split("/");
  const id = encodedID ? decodeURIComponent(encodedID) : "";
  if (request.method === "DELETE" && id) {
    saveBrowserModels(models.filter((model) => model.model_id !== id));
    return;
  }
  const returned = (await response.clone().json()) as Record<string, unknown>;
  const input = body ? (JSON.parse(body) as Record<string, unknown>) : {};
  let saved: StoredBrowserModel;
  if (action === "duplicate") {
    const source = models.find((model) => model.model_id === id);
    if (!source || typeof returned.model_id !== "string") return;
    saved = { ...source, ...returned, model_id: returned.model_id, api_key: source.api_key };
  } else if (request.method === "PUT" && id) {
    const source = models.find((model) => model.model_id === id);
    if (!source) return;
    saved = {
      ...source,
      ...input,
      ...returned,
      model_id: id,
      api_key: String(input.api_key || source.api_key),
    };
  } else if (request.method === "POST" && !id && typeof returned.model_id === "string") {
    saved = {
      ...input,
      ...returned,
      model_id: returned.model_id,
      api_key: String(input.api_key || ""),
    } as StoredBrowserModel;
  } else {
    return;
  }
  saveBrowserModels([...models.filter((model) => model.model_id !== saved.model_id), saved]);
}

async function restoreBrowserModels(): Promise<void> {
  if (!runtime) return;
  for (const model of storedBrowserModels()) {
    try {
      const response = await runtime.fetch("/api/custom-models", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(model),
      });
      if (!response.ok) console.warn(`Failed to restore browser model ${model.model_id}`);
    } catch (error) {
      console.warn(`Failed to restore browser model ${model.model_id}`, error);
    }
  }
}

function browserOpenAIRequest(apiKey: string): { api_key: string; endpoint?: string } {
  const endpoint = sessionStorage.getItem(browserOpenAITestEndpointKey);
  return endpoint ? { api_key: apiKey, endpoint } : { api_key: apiKey };
}

function isRuntimeRequest(url: URL): boolean {
  return (
    url.origin === location.origin && runtimePaths.some((path) => url.pathname.startsWith(path))
  );
}

class WasmRuntime {
  private readonly worker = new Worker(appPath("wasm-worker.js"));
  private readonly pending = new Map<number, PendingRequest>();
  private webGPUSetup: PendingWebGPUSetup | null = null;
  private localDirectorySetup: PendingLocalDirectory | null = null;
  private readonly streams = new Set<WasmEventSource>();
  private nextID = 1;
  private readyResolve!: () => void;
  private readyReject!: (error: Error) => void;
  readonly ready = new Promise<void>((resolve, reject) => {
    this.readyResolve = resolve;
    this.readyReject = reject;
  });

  constructor() {
    this.worker.addEventListener("message", (message: MessageEvent<RuntimeMessage>) => {
      const data = message.data;
      switch (data.type) {
        case "ready":
          this.readyResolve();
          return;
        case "runtime-error": {
          const error = new Error(data.error);
          this.readyReject(error);
          for (const pending of this.pending.values()) pending.reject(error);
          this.pending.clear();
          this.webGPUSetup?.reject(error);
          this.webGPUSetup = null;
          this.localDirectorySetup?.reject(error);
          this.localDirectorySetup = null;
          for (const stream of this.streams) stream.fail(error);
          return;
        }
        case "response": {
          const pending = this.pending.get(data.id);
          if (!pending) return;
          this.pending.delete(data.id);
          const contentType = Object.entries(data.response.headers || {}).find(
            ([name]) => name.toLowerCase() === "content-type",
          )?.[1];
          const body =
            typeof data.response.body === "string" && !contentType?.includes("application/json")
              ? data.response.body
              : data.response.body === undefined
                ? null
                : JSON.stringify(data.response.body);
          pending.resolve(
            new Response(body, {
              status: data.response.status,
              headers: data.response.headers,
            }),
          );
          return;
        }
        case "response-error": {
          const pending = this.pending.get(data.id);
          if (!pending) return;
          this.pending.delete(data.id);
          pending.reject(new Error(data.error));
          return;
        }
        case "stream-event":
          for (const stream of this.streams) stream.receive(data.event);
          return;
        case "webgpu-model-progress":
          this.webGPUSetup?.onProgress?.(data.progress, data.text);
          return;
        case "webgpu-model-configured":
          this.webGPUSetup?.resolve(data.model);
          this.webGPUSetup = null;
          return;
        case "webgpu-model-error":
          this.webGPUSetup?.reject(new Error(data.error));
          this.webGPUSetup = null;
          return;
        case "local-directory-progress":
          this.localDirectorySetup?.onProgress?.(data.text);
          return;
        case "local-directory-connected":
          this.localDirectorySetup?.resolve(data.info);
          this.localDirectorySetup = null;
          return;
        case "local-directory-disconnected":
          this.localDirectorySetup?.resolve({ name: "", fileCount: 0, skippedCount: 0 });
          this.localDirectorySetup = null;
          return;
        case "local-directory-error":
          this.localDirectorySetup?.reject(new Error(data.error));
          this.localDirectorySetup = null;
          return;
        case "local-directory-sync-error":
          connectedDirectory = null;
          browserDirectoryReconnect = true;
          console.error(`Local directory disconnected: ${data.error}`);
          window.dispatchEvent(
            new CustomEvent("shelley-local-directory-error", { detail: data.error }),
          );
          return;
      }
    });
  }

  async fetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    await this.ready;
    const request = new Request(input, init);
    const id = this.nextID++;
    const promise = new Promise<Response>((resolve, reject) =>
      this.pending.set(id, { resolve, reject }),
    );
    const headers: Record<string, string> = {};
    request.headers.forEach((value, name) => (headers[name] = value));
    const requestURL = new URL(request.url);
    if (request.method === "POST" && requestURL.pathname === "/api/upload") {
      const form = await request.formData();
      const file = form.get("file");
      if (!(file instanceof File)) {
        this.pending.delete(id);
        return new Response(JSON.stringify({ message: "missing file" }), {
          status: 400,
          headers: { "Content-Type": "application/json" },
        });
      }
      const bytes = await file.arrayBuffer();
      this.worker.postMessage({ type: "upload", id, name: file.name, bytes }, [bytes]);
      return promise;
    }
    const body = request.method === "GET" || request.method === "HEAD" ? "" : await request.text();
    this.worker.postMessage({
      type: "request",
      id,
      method: request.method,
      url: request.url,
      headers,
      body,
    });
    if (request.signal.aborted) {
      this.pending.delete(id);
      throw request.signal.reason ?? new DOMException("Request aborted", "AbortError");
    }
    const response = await promise;
    await rememberBrowserModel(request, body, response);
    return response;
  }

  addStream(stream: WasmEventSource): void {
    this.streams.add(stream);
    void this.ready.then(
      () => stream.open(),
      (error) => stream.fail(error),
    );
  }

  removeStream(stream: WasmEventSource): void {
    this.streams.delete(stream);
  }

  async configureWebGPUModel(
    onProgress?: (progress: number, text: string) => void,
  ): Promise<string> {
    await this.ready;
    if (this.webGPUSetup) throw new Error("A local model is already loading");
    const id = this.nextID++;
    const promise = new Promise<string>((resolve, reject) => {
      this.webGPUSetup = { resolve, reject, onProgress };
    });
    this.worker.postMessage({ type: "configure-webgpu-model", id });
    return promise;
  }

  async connectLocalDirectory(
    handle: FileSystemDirectoryHandle,
    onProgress?: (text: string) => void,
  ): Promise<BrowserDirectoryInfo> {
    await this.ready;
    if (this.localDirectorySetup) throw new Error("A local folder operation is already running");
    const id = this.nextID++;
    const promise = new Promise<BrowserDirectoryInfo>((resolve, reject) => {
      this.localDirectorySetup = { resolve, reject, onProgress };
    });
    this.worker.postMessage({ type: "connect-local-directory", id, handle });
    return promise;
  }

  async disconnectLocalDirectory(): Promise<void> {
    await this.ready;
    if (this.localDirectorySetup) throw new Error("A local folder operation is already running");
    const id = this.nextID++;
    const promise = new Promise<BrowserDirectoryInfo>((resolve, reject) => {
      this.localDirectorySetup = { resolve, reject };
    });
    this.worker.postMessage({ type: "disconnect-local-directory", id });
    await promise;
  }
}

let runtime: WasmRuntime | null = null;

class WasmEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readonly CONNECTING = 0;
  readonly OPEN = 1;
  readonly CLOSED = 2;
  readonly url: string;
  readonly withCredentials = false;
  readyState = WasmEventSource.CONNECTING;
  onopen: ((this: EventSource, ev: Event) => unknown) | null = null;
  onmessage: ((this: EventSource, ev: MessageEvent) => unknown) | null = null;
  onerror: ((this: EventSource, ev: Event) => unknown) | null = null;

  constructor(url: string | URL) {
    super();
    this.url = String(url);
    if (!runtime) throw new Error("WASM runtime is not installed");
    runtime.addStream(this);
  }

  open(): void {
    if (this.readyState === WasmEventSource.CLOSED) return;
    this.readyState = WasmEventSource.OPEN;
    const event = new Event("open");
    this.onopen?.call(this as unknown as EventSource, event);
    this.dispatchEvent(event);
  }

  receive(data: string): void {
    if (this.readyState !== WasmEventSource.OPEN) return;
    const event = new MessageEvent("message", { data });
    this.onmessage?.call(this as unknown as EventSource, event);
    this.dispatchEvent(event);
  }

  fail(error: Error): void {
    void error;
    if (this.readyState === WasmEventSource.CLOSED) return;
    const event = new Event("error");
    this.onerror?.call(this as unknown as EventSource, event);
    this.dispatchEvent(event);
  }

  close(): void {
    if (this.readyState === WasmEventSource.CLOSED) return;
    this.readyState = WasmEventSource.CLOSED;
    runtime?.removeStream(this);
  }
}

// installWasmRuntime routes Shelley's existing HTTP and EventSource call sites
// through one dedicated worker. Static assets continue through native fetch.
export async function installWasmRuntime(): Promise<void> {
  if (runtime) return runtime.ready;
  runtime = new WasmRuntime();
  const nativeFetch = globalThis.fetch.bind(globalThis);
  globalThis.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
    const requestURL = new URL(input instanceof Request ? input.url : String(input), location.href);
    if (!isRuntimeRequest(requestURL)) return nativeFetch(input, init);
    if (!runtime) throw new Error("WASM runtime is not installed");
    const request = new Request(input, init);
    return runtime.fetch(request);
  };
  globalThis.EventSource = WasmEventSource as unknown as typeof EventSource;
  await runtime.ready;

  if (browserDirectoryHandles.pickerSupported()) {
    try {
      rememberedDirectoryHandle = await browserDirectoryHandles.load();
      if (rememberedDirectoryHandle) {
        const permission = await browserDirectoryHandles.permission(rememberedDirectoryHandle);
        if (permission === "granted") {
          connectedDirectory = await runtime.connectLocalDirectory(rememberedDirectoryHandle);
        } else {
          browserDirectoryReconnect = true;
        }
      }
    } catch (error) {
      console.warn("Failed to restore the local directory", error);
      browserDirectoryReconnect = rememberedDirectoryHandle !== null;
    }
  }

  const selectedBackend = localStorage.getItem(browserModelBackendKey);
  if (selectedBackend === "webgpu") {
    try {
      await configureBrowserWebGPUModel();
    } catch {
      localStorage.removeItem(browserModelBackendKey);
    }
  } else {
    const storedOpenAIKey = localStorage.getItem(browserOpenAIKeyStorageKey);
    if (storedOpenAIKey) {
      try {
        await configureBrowserOpenAIKey(storedOpenAIKey);
      } catch {
        // Keep loading the app so the connection dialog can replace a stale or
        // invalid key. The failed key remains available for a later retry.
      }
    }
  }

  await restoreBrowserModels();

  const selectedDirectory = localStorage.getItem("shelley_selected_cwd");
  if (selectedDirectory !== "/workspace" && !selectedDirectory?.startsWith("/workspace/")) {
    localStorage.setItem("shelley_selected_cwd", "/workspace");
  }

  const modelResponse = await runtime.fetch("/api/models");
  if (!modelResponse.ok) throw new Error(`Failed to load browser models: ${modelResponse.status}`);
  const models = (await modelResponse.json()) as Array<{
    id: string;
    ready: boolean;
    is_default?: boolean;
    [key: string]: unknown;
  }>;
  const readyModels = models.filter((model) => model.ready);
  window.__SHELLEY_INIT__ = {
    models,
    default_model: readyModels.find((model) => model.is_default)?.id || readyModels[0]?.id || "",
    default_cwd: "/workspace",
    home_dir: "/workspace",
    hostname: "browser",
    user_agents_md_path: "/workspace/AGENTS.md",
    notification_channel_types: [],
    banner:
      "Browser runtime: conversations stay in this browser. Folder access is limited to directories you choose.",
  };
}

export function browserOpenAIKeyRequired(): boolean {
  if (new URLSearchParams(location.search).get("model") === "predictable") {
    sessionStorage.setItem(browserPredictableModelKey, "1");
  }
  return (
    sessionStorage.getItem("shelley_runtime") === "wasm" &&
    (browserDirectoryReconnect ||
      (!sessionStorage.getItem(browserPredictableModelKey) &&
        !browserOpenAIConfigured &&
        !browserWebGPUConfigured))
  );
}

export function browserLocalDirectorySupported(): boolean {
  return browserDirectoryHandles.pickerSupported();
}

export function browserLocalDirectoryReconnectRequired(): boolean {
  return browserDirectoryReconnect;
}

export function browserModelConfigured(): boolean {
  return (
    Boolean(sessionStorage.getItem(browserPredictableModelKey)) ||
    browserOpenAIConfigured ||
    browserWebGPUConfigured
  );
}

export function browserConnectedDirectory(): BrowserDirectoryInfo | null {
  return connectedDirectory;
}

export async function connectBrowserLocalDirectory(
  onProgress?: (text: string) => void,
): Promise<BrowserDirectoryInfo> {
  if (!runtime) throw new Error("WASM runtime is not installed");
  let handle = rememberedDirectoryHandle;
  if (handle && browserDirectoryReconnect) {
    // The handle is already in memory, so permission is the first awaited
    // browser operation and still carries the button click's user activation.
    await browserDirectoryHandles.requestPermission(handle);
  } else {
    handle = await browserDirectoryHandles.pick();
  }
  const info = await runtime.connectLocalDirectory(handle, onProgress);
  rememberedDirectoryHandle = handle;
  connectedDirectory = info;
  browserDirectoryReconnect = false;
  try {
    await browserDirectoryHandles.remember(handle);
  } catch (error) {
    console.warn("The local directory is connected but could not be remembered", error);
  }
  return info;
}

export async function useBrowserWorkspaceInstead(): Promise<void> {
  if (!runtime) throw new Error("WASM runtime is not installed");
  await runtime.disconnectLocalDirectory();
  await browserDirectoryHandles.forget();
  rememberedDirectoryHandle = null;
  connectedDirectory = null;
  browserDirectoryReconnect = false;
}

export async function configureBrowserWebGPUModel(
  onProgress?: (progress: number, text: string) => void,
): Promise<string> {
  if (!runtime) throw new Error("WASM runtime is not installed");
  const model = await runtime.configureWebGPUModel(onProgress);
  browserWebGPUConfigured = true;
  browserOpenAIConfigured = false;
  localStorage.setItem(browserModelBackendKey, "webgpu");
  sessionStorage.removeItem(browserPredictableModelKey);
  return model;
}

export async function configureBrowserOpenAIKey(apiKey: string): Promise<string> {
  const normalized = apiKey.trim();
  if (!normalized) throw new Error("Enter an OpenAI API key");
  const response = await fetch("/api/browser-openai-key", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(browserOpenAIRequest(normalized)),
  });
  const body = (await response.json().catch(() => ({}))) as {
    error?: string;
    models?: Array<{ id: string; is_default?: boolean }>;
  };
  if (!response.ok) {
    throw new Error(body.error || `OpenAI setup failed: ${response.status}`);
  }
  localStorage.setItem(browserOpenAIKeyStorageKey, normalized);
  localStorage.setItem(browserModelBackendKey, "openai");
  browserOpenAIConfigured = true;
  browserWebGPUConfigured = false;
  sessionStorage.removeItem(browserPredictableModelKey);
  return (
    body.models?.find((model) => model.is_default)?.id || body.models?.[0]?.id || "gpt-5.6-luna"
  );
}
