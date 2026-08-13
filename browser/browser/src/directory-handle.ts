type FileSystemPermissionMode = "read" | "readwrite";

type PermissionCapableDirectoryHandle = FileSystemDirectoryHandle & {
  queryPermission(options?: {
    mode?: FileSystemPermissionMode;
  }): Promise<PermissionState>;
  requestPermission(options?: {
    mode?: FileSystemPermissionMode;
  }): Promise<PermissionState>;
};

type DirectoryPickerWindow = Window & {
  showDirectoryPicker?: (options?: {
    id?: string;
    mode?: FileSystemPermissionMode;
    startIn?: FileSystemHandle | string;
  }) => Promise<FileSystemDirectoryHandle>;
};

export type BrowserDirectoryHandleStoreOptions = {
  databaseName: string;
  storeName?: string;
  handleKey?: string;
  pickerID?: string;
};

// BrowserDirectoryHandleStore owns permission and persistence mechanics for a
// user-selected File System Access API directory. The caller chooses storage
// names so adopting the package never requires a migration.
export class BrowserDirectoryHandleStore {
  private readonly storeName: string;
  private readonly handleKey: string;
  private readonly pickerID: string;

  constructor(private readonly options: BrowserDirectoryHandleStoreOptions) {
    this.storeName = options.storeName ?? "handles";
    this.handleKey = options.handleKey ?? "workspace";
    this.pickerID = options.pickerID ?? "agent-workspace";
  }

  private openDatabase(): Promise<IDBDatabase> {
    return new Promise((resolve, reject) => {
      const request = indexedDB.open(this.options.databaseName, 1);
      request.onupgradeneeded = () =>
        request.result.createObjectStore(this.storeName);
      request.onsuccess = () => resolve(request.result);
      request.onerror = () =>
        reject(
          request.error ?? new Error("failed to open local-directory storage"),
        );
    });
  }

  pickerSupported(): boolean {
    return (
      typeof (window as DirectoryPickerWindow).showDirectoryPicker ===
      "function"
    );
  }

  async pick(): Promise<FileSystemDirectoryHandle> {
    const picker = (window as DirectoryPickerWindow).showDirectoryPicker;
    if (!picker)
      throw new Error(
        "Opening a local folder requires Chrome or Edge on desktop",
      );

    // Keep the picker as the first awaited operation so Chrome sees the
    // button's transient user activation.
    const handle = await picker.call(window, {
      id: this.pickerID,
      mode: "readwrite",
    });
    await this.requestPermission(handle);
    return handle;
  }

  async remember(handle: FileSystemDirectoryHandle): Promise<void> {
    const database = await this.openDatabase();
    try {
      await new Promise<void>((resolve, reject) => {
        const transaction = database.transaction(this.storeName, "readwrite");
        transaction.objectStore(this.storeName).put(handle, this.handleKey);
        transaction.oncomplete = () => resolve();
        transaction.onerror = () =>
          reject(
            transaction.error ??
              new Error("failed to remember local directory"),
          );
      });
    } finally {
      database.close();
    }
  }

  async load(): Promise<FileSystemDirectoryHandle | null> {
    const database = await this.openDatabase();
    try {
      return await new Promise((resolve, reject) => {
        const request = database
          .transaction(this.storeName, "readonly")
          .objectStore(this.storeName)
          .get(this.handleKey);
        request.onsuccess = () => {
          const handle = request.result as
            | FileSystemDirectoryHandle
            | undefined;
          resolve(handle?.kind === "directory" ? handle : null);
        };
        request.onerror = () =>
          reject(
            request.error ?? new Error("failed to restore local directory"),
          );
      });
    } finally {
      database.close();
    }
  }

  permission(handle: FileSystemDirectoryHandle): Promise<PermissionState> {
    return (handle as PermissionCapableDirectoryHandle).queryPermission({
      mode: "readwrite",
    });
  }

  async requestPermission(handle: FileSystemDirectoryHandle): Promise<void> {
    const permission = await (
      handle as PermissionCapableDirectoryHandle
    ).requestPermission({
      mode: "readwrite",
    });
    if (permission !== "granted") {
      throw new Error("Read and write access to the folder is required");
    }
  }

  async forget(): Promise<void> {
    const database = await this.openDatabase();
    try {
      await new Promise<void>((resolve, reject) => {
        const transaction = database.transaction(this.storeName, "readwrite");
        transaction.objectStore(this.storeName).delete(this.handleKey);
        transaction.oncomplete = () => resolve();
        transaction.onerror = () =>
          reject(
            transaction.error ?? new Error("failed to forget local directory"),
          );
      });
    } finally {
      database.close();
    }
  }
}
