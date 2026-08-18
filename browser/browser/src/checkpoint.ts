export type IndexedDBCheckpointStoreOptions = {
  openDatabase: () => Promise<IDBDatabase>;
  checkpointStore: string;
  writeStore: string;
};

export type IndexedDBCheckpointStore = (
  operation: string,
  payload: string,
) => Promise<string>;

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

type CheckpointConfig = {
  thread_id: string;
  checkpoint_ns?: string;
  checkpoint_id?: string;
};

type CheckpointListRequest = {
  config?: CheckpointConfig;
  metadata?: Record<string, unknown>;
  before?: CheckpointConfig;
  after?: CheckpointConfig;
  limit: number;
  all_namespaces?: boolean;
};

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

function compareCheckpointPosition(
  left: CheckpointRecord,
  right: CheckpointRecord | CheckpointConfig,
): number {
  if (left.thread_id !== right.thread_id)
    return left.thread_id < right.thread_id ? -1 : 1;
  const rightNamespace = "namespace" in right ? right.namespace : right.checkpoint_ns || "";
  if (left.namespace !== rightNamespace)
    return left.namespace < rightNamespace ? -1 : 1;
  const rightID = "namespace" in right ? right.checkpoint_id : right.checkpoint_id || "";
  if (left.checkpoint_id === rightID) return 0;
  return left.checkpoint_id > rightID ? -1 : 1;
}

function equalJSON(left: unknown, right: unknown): boolean {
  if (Object.is(left, right)) return true;
  if (Array.isArray(left) || Array.isArray(right)) {
    return (
      Array.isArray(left) &&
      Array.isArray(right) &&
      left.length === right.length &&
      left.every((value, index) => equalJSON(value, right[index]))
    );
  }
  if (
    left === null ||
    right === null ||
    typeof left !== "object" ||
    typeof right !== "object"
  )
    return false;
  const leftObject = left as Record<string, unknown>;
  const rightObject = right as Record<string, unknown>;
  const leftKeys = Object.keys(leftObject);
  const rightKeys = Object.keys(rightObject);
  return (
    leftKeys.length === rightKeys.length &&
    leftKeys.every(
      (key) =>
        Object.prototype.hasOwnProperty.call(rightObject, key) &&
        equalJSON(leftObject[key], rightObject[key]),
    )
  );
}

function checkpointMetadataMatches(
  record: CheckpointRecord,
  filter: Record<string, unknown> | undefined,
): boolean {
  if (!filter || Object.keys(filter).length === 0) return true;
  const metadata = (record.metadata || {}) as Record<string, unknown>;
  const values: Record<string, unknown> = {
    source: metadata.source || "",
    step: metadata.step || 0,
    run_id: metadata.run_id || "",
    ...((metadata.extra || {}) as Record<string, unknown>),
  };
  return Object.entries(filter).every(([key, value]) =>
    equalJSON(values[key], value),
  );
}

async function recordsForThread<T>(
  database: IDBDatabase,
  storeName: string,
  threadID: string,
): Promise<T[]> {
  const transaction = database.transaction(storeName, "readonly");
  return requestResult<T[]>(
    transaction
      .objectStore(storeName)
      .index("by_thread")
      .getAll(IDBKeyRange.only(threadID)),
  );
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

// createIndexedDBCheckpointStore implements the normalized operation protocol
// used by browser/checkpoint. The caller owns database creation and versioning.
export function createIndexedDBCheckpointStore(
  options: IndexedDBCheckpointStoreOptions,
): IndexedDBCheckpointStore {
  return async (operation: string, encoded: string): Promise<string> => {
    const payload = JSON.parse(encoded || "null") as Record<
      string,
      unknown
    > | null;
    const database = await options.openDatabase();
    try {
      switch (operation) {
        case "put_checkpoint": {
          const transaction = database.transaction(
            options.checkpointStore,
            "readwrite",
          );
          transaction
            .objectStore(options.checkpointStore)
            .put(payload as CheckpointRecord);
          await transactionDone(transaction);
          return "";
        }
        case "put_writes": {
          const records = (payload?.writes || []) as CheckpointWriteRecord[];
          const transaction = database.transaction(
            options.writeStore,
            "readwrite",
          );
          const store = transaction.objectStore(options.writeStore);
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
          const transaction = database.transaction(
            options.checkpointStore,
            "readonly",
          );
          const store = transaction.objectStore(options.checkpointStore);
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
              !result || record.checkpoint_id > result.checkpoint_id
                ? record
                : result,
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
          const transaction = database.transaction(
            options.writeStore,
            "readonly",
          );
          const records = await requestResult<CheckpointWriteRecord[]>(
            transaction
              .objectStore(options.writeStore)
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
          const request = payload as CheckpointListRequest;
          if (!Number.isInteger(request.limit) || request.limit <= 0)
            throw new Error("checkpoint list limit must be a positive integer");
          const config = request.config;
          const transaction = database.transaction(
            options.checkpointStore,
            "readonly",
          );
          const store = transaction.objectStore(options.checkpointStore);
          const cursorRequest = config?.thread_id
            ? store
                .index("by_thread")
                .openCursor(IDBKeyRange.only(config.thread_id))
            : store.openCursor();
          const records: CheckpointRecord[] = [];
          await new Promise<void>((resolve, reject) => {
            cursorRequest.onerror = () =>
              reject(
                cursorRequest.error ??
                  new Error("IndexedDB checkpoint cursor failed"),
              );
            cursorRequest.onsuccess = () => {
              const cursor = cursorRequest.result;
              if (!cursor) {
                resolve();
                return;
              }
              const record = cursor.value as CheckpointRecord;
              const namespace = config?.checkpoint_ns || "";
              const matches =
                (!config ||
                  (record.thread_id === config.thread_id &&
                    (request.all_namespaces ||
                      record.namespace === namespace) &&
                    (!config.checkpoint_id ||
                      record.checkpoint_id === config.checkpoint_id))) &&
                (!request.before?.checkpoint_id ||
                  record.checkpoint_id < request.before.checkpoint_id) &&
                (!request.after ||
                  compareCheckpointPosition(record, request.after) > 0) &&
                checkpointMetadataMatches(record, request.metadata);
              if (matches) {
                records.push(record);
                records.sort(compareCheckpointPosition);
                if (records.length > request.limit) records.pop();
              }
              cursor.continue();
            };
          });
          return JSON.stringify(records);
        }
        case "delete_thread": {
          const threadID = String(payload?.thread_id || "");
          const transaction = database.transaction(
            [options.checkpointStore, options.writeStore],
            "readwrite",
          );
          deleteIndexMatches(
            transaction.objectStore(options.checkpointStore),
            "by_thread",
            IDBKeyRange.only(threadID),
          );
          deleteIndexMatches(
            transaction.objectStore(options.writeStore),
            "by_thread",
            IDBKeyRange.only(threadID),
          );
          await transactionDone(transaction);
          return "";
        }
        case "copy_thread": {
          const source = String(payload?.source_thread_id || "");
          const target = String(payload?.target_thread_id || "");
          const [sourceCheckpoints, sourceWrites, targetCheckpoints] =
            await Promise.all([
              recordsForThread<CheckpointRecord>(
                database,
                options.checkpointStore,
                source,
              ),
              recordsForThread<CheckpointWriteRecord>(
                database,
                options.writeStore,
                source,
              ),
              recordsForThread<CheckpointRecord>(
                database,
                options.checkpointStore,
                target,
              ),
            ]);
          if (targetCheckpoints.length > 0) {
            throw new Error(
              `checkpoint target ${JSON.stringify(target)} already exists`,
            );
          }
          const transaction = database.transaction(
            [options.checkpointStore, options.writeStore],
            "readwrite",
          );
          const checkpoints = transaction.objectStore(options.checkpointStore);
          const writes = transaction.objectStore(options.writeStore);
          for (const record of sourceCheckpoints)
            checkpoints.put({ ...record, thread_id: target });
          for (const record of sourceWrites)
            writes.put({ ...record, thread_id: target });
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
            [options.checkpointStore, options.writeStore],
            "readwrite",
          );
          const checkpoints = transaction.objectStore(options.checkpointStore);
          const writes = transaction.objectStore(options.writeStore);
          for (const config of configs) {
            const key = [
              config.thread_id,
              config.checkpoint_ns || "",
              config.checkpoint_id,
            ];
            checkpoints.delete(key);
            deleteIndexMatches(writes, "by_checkpoint", IDBKeyRange.only(key));
          }
          await transactionDone(transaction);
          return "";
        }
        default:
          throw new Error(
            `unsupported checkpoint operation ${JSON.stringify(operation)}`,
          );
      }
    } finally {
      database.close();
    }
  };
}
