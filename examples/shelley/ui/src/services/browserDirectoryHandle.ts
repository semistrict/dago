type FileSystemPermissionMode = "read" | "readwrite";

type PermissionCapableDirectoryHandle = FileSystemDirectoryHandle & {
  queryPermission(options?: { mode?: FileSystemPermissionMode }): Promise<PermissionState>;
  requestPermission(options?: { mode?: FileSystemPermissionMode }): Promise<PermissionState>;
};

type DirectoryPickerWindow = Window & {
  showDirectoryPicker?: (options?: {
    id?: string;
    mode?: FileSystemPermissionMode;
    startIn?: FileSystemHandle | string;
  }) => Promise<FileSystemDirectoryHandle>;
};

const databaseName = "shelley-local-directory";
const storeName = "handles";
const handleKey = "workspace";

function openDatabase(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(databaseName, 1);
    request.onupgradeneeded = () => request.result.createObjectStore(storeName);
    request.onsuccess = () => resolve(request.result);
    request.onerror = () =>
      reject(request.error ?? new Error("failed to open local-directory storage"));
  });
}

async function storeHandle(handle: FileSystemDirectoryHandle): Promise<void> {
  const database = await openDatabase();
  try {
    await new Promise<void>((resolve, reject) => {
      const transaction = database.transaction(storeName, "readwrite");
      transaction.objectStore(storeName).put(handle, handleKey);
      transaction.oncomplete = () => resolve();
      transaction.onerror = () =>
        reject(transaction.error ?? new Error("failed to remember local directory"));
    });
  } finally {
    database.close();
  }
}

export function browserDirectoryPickerSupported(): boolean {
  return typeof (window as DirectoryPickerWindow).showDirectoryPicker === "function";
}

export async function pickBrowserDirectory(): Promise<FileSystemDirectoryHandle> {
  const picker = (window as DirectoryPickerWindow).showDirectoryPicker;
  if (!picker) throw new Error("Opening a local folder requires Chrome or Edge on desktop");

  // Keep the picker as the first awaited operation so Chrome sees the button's
  // transient user activation.
  const handle = await picker.call(window, { id: "shelley-workspace", mode: "readwrite" });
  await requestBrowserDirectoryPermission(handle);
  return handle;
}

export async function rememberBrowserDirectory(handle: FileSystemDirectoryHandle): Promise<void> {
  await storeHandle(handle);
}

export async function loadRememberedBrowserDirectory(): Promise<FileSystemDirectoryHandle | null> {
  const database = await openDatabase();
  try {
    return await new Promise((resolve, reject) => {
      const request = database
        .transaction(storeName, "readonly")
        .objectStore(storeName)
        .get(handleKey);
      request.onsuccess = () => {
        const handle = request.result as FileSystemDirectoryHandle | undefined;
        resolve(handle?.kind === "directory" ? handle : null);
      };
      request.onerror = () =>
        reject(request.error ?? new Error("failed to restore local directory"));
    });
  } finally {
    database.close();
  }
}

export async function browserDirectoryPermission(
  handle: FileSystemDirectoryHandle,
): Promise<PermissionState> {
  return (handle as PermissionCapableDirectoryHandle).queryPermission({ mode: "readwrite" });
}

export async function requestBrowserDirectoryPermission(
  handle: FileSystemDirectoryHandle,
): Promise<void> {
  const permission = await (handle as PermissionCapableDirectoryHandle).requestPermission({
    mode: "readwrite",
  });
  if (permission !== "granted") throw new Error("Read and write access to the folder is required");
}

export async function forgetBrowserDirectory(): Promise<void> {
  const database = await openDatabase();
  try {
    await new Promise<void>((resolve, reject) => {
      const transaction = database.transaction(storeName, "readwrite");
      transaction.objectStore(storeName).delete(handleKey);
      transaction.oncomplete = () => resolve();
      transaction.onerror = () =>
        reject(transaction.error ?? new Error("failed to forget local directory"));
    });
  } finally {
    database.close();
  }
}
