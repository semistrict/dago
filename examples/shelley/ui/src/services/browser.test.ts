import assert from "node:assert/strict";
import "fake-indexeddb/auto";
import { createBrowserFileStore } from "@semistrict/browser/indexeddb";
import { WasmFileSystemAdapter } from "@semistrict/browser/filesystem";

const databaseName = `browser-test-${crypto.randomUUID()}`;

function openDatabase(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(databaseName, 1);
    request.onupgradeneeded = () => request.result.createObjectStore("files", { keyPath: "path" });
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

async function readKeys(): Promise<IDBValidKey[]> {
  const database = await openDatabase();
  try {
    const transaction = database.transaction("files", "readonly");
    return await new Promise((resolve, reject) => {
      const request = transaction.objectStore("files").getAllKeys();
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
  } finally {
    database.close();
  }
}

async function testRecordOrientedStore(): Promise<void> {
  const store = createBrowserFileStore({ openDatabase, storeName: "files" });
  await store(
    "put",
    JSON.stringify({
      path: "/workspace/example.txt",
      value: { content: "body stays independently addressable" },
      size: 37,
      mode: 420,
    }),
  );
  assert.deepEqual((await readKeys()).sort(), [
    "/workspace/example.txt",
    "::metadata::/workspace/example.txt",
  ]);

  const observedGets: IDBValidKey[] = [];
  const originalGet = IDBObjectStore.prototype.get;
  IDBObjectStore.prototype.get = function (query: IDBValidKey | IDBKeyRange) {
    observedGets.push(query as IDBValidKey);
    return originalGet.call(this, query);
  };
  try {
    const metadata = JSON.parse(await store("metadata", "")) as Array<Record<string, unknown>>;
    assert.equal(metadata.length, 1);
    assert.equal(metadata[0].path, "/workspace/example.txt");
    assert.equal(metadata[0].size, 37);
    assert.equal("value" in metadata[0], false);
    assert.deepEqual(observedGets, ["::metadata::/workspace/example.txt"]);
  } finally {
    IDBObjectStore.prototype.get = originalGet;
  }

  const body = JSON.parse(await store("get", JSON.stringify({ path: "/workspace/example.txt" })));
  assert.equal(body.value.content, "body stays independently addressable");
  await store("delete", JSON.stringify({ path: "/workspace/example.txt" }));
  assert.deepEqual(await readKeys(), []);
}

async function testConfigurableFilesystemBridge(): Promise<void> {
  const calls: Array<{ operation: string; payload: Record<string, unknown> }> = [];
  const adapter = new WasmFileSystemAdapter({
    workspaceRoot: "/agent",
    paths: () => JSON.stringify(["/agent", "/agent/note.txt"]),
    execute: async (operation, encoded) => {
      const payload = JSON.parse(encoded) as Record<string, unknown>;
      calls.push({ operation, payload });
      if (operation === "exists") return "true";
      return "";
    },
  });
  assert.equal(await adapter.exists("note.txt"), true);
  assert.deepEqual(calls, [{ operation: "exists", payload: { path: "/agent/note.txt" } }]);
  assert.deepEqual(adapter.getAllPaths(), ["/", "/note.txt"]);
}

await testRecordOrientedStore();
await testConfigurableFilesystemBridge();
console.log("browser package tests passed");
