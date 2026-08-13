export type BrowserFileStoreOptions = {
  openDatabase: () => Promise<IDBDatabase>;
  storeName: string;
  metadataPrefix?: string;
};

export type BrowserFileStore = (
  operation: string,
  payload: string,
) => Promise<string>;

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () =>
      reject(request.error ?? new Error("IndexedDB request failed"));
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

// createBrowserFileStore returns the byte-store bridge consumed by the Go
// browser filesystem. File bodies and metadata sidecars are separate records;
// metadata discovery uses keys and sidecars without reading file bodies.
export function createBrowserFileStore(
  options: BrowserFileStoreOptions,
): BrowserFileStore {
  const metadataPrefix = options.metadataPrefix ?? "::metadata::";
  return async (operation: string, encoded: string): Promise<string> => {
    const payload = encoded
      ? (JSON.parse(encoded) as Record<string, unknown>)
      : {};
    const database = await options.openDatabase();
    try {
      switch (operation) {
        case "metadata": {
          const transaction = database.transaction(
            options.storeName,
            "readonly",
          );
          const store = transaction.objectStore(options.storeName);
          const keys = await requestResult<IDBValidKey[]>(store.getAllKeys());
          const metadataKeys = keys.filter(
            (key): key is string =>
              typeof key === "string" && key.startsWith(metadataPrefix),
          );
          const storedMetadata = await Promise.all(
            metadataKeys.map((key) =>
              requestResult<Record<string, unknown>>(store.get(key)),
            ),
          );
          const records: Array<Record<string, unknown>> = storedMetadata.map(
            ({ file_path, ...record }) => ({ ...record, path: file_path }),
          );
          const described = new Set(records.map((record) => record.path));
          for (const key of keys) {
            if (
              typeof key !== "string" ||
              key.startsWith(metadataPrefix) ||
              described.has(key)
            ) {
              continue;
            }
            records.push({ path: key, size: 0 });
          }
          await transactionDone(transaction);
          return JSON.stringify(records);
        }
        case "get": {
          const transaction = database.transaction(
            options.storeName,
            "readonly",
          );
          const record = await requestResult<
            Record<string, unknown> | undefined
          >(
            transaction
              .objectStore(options.storeName)
              .get(payload.path as string),
          );
          await transactionDone(transaction);
          return record ? JSON.stringify(record) : "";
        }
        case "put": {
          const transaction = database.transaction(
            options.storeName,
            "readwrite",
          );
          const store = transaction.objectStore(options.storeName);
          store.put(payload);
          const metadata = { ...payload };
          delete metadata.value;
          store.put({
            ...metadata,
            path: `${metadataPrefix}${String(payload.path)}`,
            file_path: payload.path,
          });
          await transactionDone(transaction);
          return "";
        }
        case "delete": {
          const transaction = database.transaction(
            options.storeName,
            "readwrite",
          );
          const store = transaction.objectStore(options.storeName);
          store.delete(payload.path as string);
          store.delete(`${metadataPrefix}${String(payload.path)}`);
          await transactionDone(transaction);
          return "";
        }
        default:
          throw new Error(
            `unsupported browser file store operation ${JSON.stringify(operation)}`,
          );
      }
    } finally {
      database.close();
    }
  };
}
