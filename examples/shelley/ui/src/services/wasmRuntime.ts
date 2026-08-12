import { appPath } from "../basePath";

type PendingRequest = {
  resolve: (response: Response) => void;
  reject: (error: Error) => void;
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
  | { type: "stream-event"; event: string };

const runtimePaths = ["/api/", "/feature-flags", "/version-check", "/upgrade", "/exit"];
const browserLocalModelKey = "shelley_wasm_local_model";
const browserOpenAIKeyStorageKey = "shelley_wasm_openai_key";
const browserOpenAITestEndpointKey = "shelley_wasm_openai_test_endpoint";
let browserOpenAIConfigured = false;

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
          for (const stream of this.streams) stream.fail(error);
          return;
        }
        case "response": {
          const pending = this.pending.get(data.id);
          if (!pending) return;
          this.pending.delete(data.id);
          const body = data.response.body === undefined ? null : JSON.stringify(data.response.body);
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
    this.worker.postMessage({
      type: "request",
      id,
      method: request.method,
      url: request.url,
      headers,
      body: request.method === "GET" || request.method === "HEAD" ? "" : await request.text(),
    });
    if (request.signal.aborted) {
      this.pending.delete(id);
      throw request.signal.reason ?? new DOMException("Request aborted", "AbortError");
    }
    return promise;
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

  const storedOpenAIKey = localStorage.getItem(browserOpenAIKeyStorageKey);
  if (storedOpenAIKey) {
    try {
      await configureBrowserOpenAIKey(storedOpenAIKey);
    } catch {
      // Keep loading the app so the connection dialog can replace a stale or
      // invalid key. The failed key remains available for a later retry.
    }
  }

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
    notification_channel_types: [],
    banner: "Browser runtime: conversations and workspace files stay in this browser.",
  };
}

export function browserOpenAIKeyRequired(): boolean {
  if (new URLSearchParams(location.search).get("model") === "local") {
    sessionStorage.setItem(browserLocalModelKey, "1");
  }
  return (
    sessionStorage.getItem("shelley_runtime") === "wasm" &&
    !sessionStorage.getItem(browserLocalModelKey) &&
    !browserOpenAIConfigured
  );
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
  browserOpenAIConfigured = true;
  sessionStorage.removeItem(browserLocalModelKey);
  return (
    body.models?.find((model) => model.is_default)?.id || body.models?.[0]?.id || "gpt-5.6-luna"
  );
}
