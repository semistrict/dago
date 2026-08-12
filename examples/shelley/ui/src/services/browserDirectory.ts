export type BrowserWorkspaceFile = {
  content: string;
  encoding: "utf-8" | "base64";
};

export type BrowserDirectoryInfo = {
  name: string;
  fileCount: number;
  skippedCount: number;
};

type DirectoryHandle = FileSystemDirectoryHandle & {
  entries(): AsyncIterableIterator<[string, FileSystemHandle]>;
};

const workspaceRoot = "/workspace/";
const ignoredDirectories = new Set([".git", "node_modules"]);
const maximumFiles = 20_000;
const maximumFileBytes = 8 * 1024 * 1024;
const maximumWorkspaceBytes = 128 * 1024 * 1024;

function bytesToBase64(value: Uint8Array): string {
  let encoded = "";
  for (let index = 0; index < value.length; index += 0x8000) {
    encoded += String.fromCharCode(...value.subarray(index, index + 0x8000));
  }
  return btoa(encoded);
}

function base64ToBytes(value: string): Uint8Array {
  const decoded = atob(value);
  const bytes = new Uint8Array(decoded.length);
  for (let index = 0; index < decoded.length; index++) bytes[index] = decoded.charCodeAt(index);
  return bytes;
}

function encodeFile(bytes: Uint8Array): BrowserWorkspaceFile {
  try {
    return {
      content: new TextDecoder("utf-8", { fatal: true }).decode(bytes),
      encoding: "utf-8",
    };
  } catch {
    return { content: bytesToBase64(bytes), encoding: "base64" };
  }
}

function decodeFile(file: BrowserWorkspaceFile): ArrayBuffer {
  const bytes =
    file.encoding === "base64"
      ? base64ToBytes(file.content)
      : new TextEncoder().encode(file.content);
  const buffer = new ArrayBuffer(bytes.byteLength);
  new Uint8Array(buffer).set(bytes);
  return buffer;
}

function decodedFileSize(file: BrowserWorkspaceFile): number {
  return file.encoding === "base64"
    ? Math.floor((file.content.length * 3) / 4) -
        (file.content.endsWith("==") ? 2 : file.content.endsWith("=") ? 1 : 0)
    : new TextEncoder().encode(file.content).byteLength;
}

function sameFile(left: BrowserWorkspaceFile | undefined, right: BrowserWorkspaceFile): boolean {
  return left?.encoding === right.encoding && left.content === right.content;
}

function relativePath(workspacePath: string): string[] {
  if (!workspacePath.startsWith(workspaceRoot)) {
    throw new Error(`Workspace path is outside ${workspaceRoot.slice(0, -1)}: ${workspacePath}`);
  }
  const parts = workspacePath.slice(workspaceRoot.length).split("/").filter(Boolean);
  if (!parts.length || parts.some((part) => part === "." || part === "..")) {
    throw new Error(`Invalid workspace path: ${workspacePath}`);
  }
  return parts;
}

async function parentDirectory(
  root: FileSystemDirectoryHandle,
  parts: string[],
  create: boolean,
): Promise<FileSystemDirectoryHandle> {
  let directory = root;
  for (const part of parts.slice(0, -1)) {
    directory = await directory.getDirectoryHandle(part, { create });
  }
  return directory;
}

export class BrowserDirectoryWorkspace {
  private handle: FileSystemDirectoryHandle | null = null;
  private baseline: Record<string, BrowserWorkspaceFile> = {};
  private excludedPaths = new Set<string>();
  private tail = Promise.resolve();

  connect(
    handle: FileSystemDirectoryHandle,
  ): Promise<{ files: Record<string, BrowserWorkspaceFile>; info: BrowserDirectoryInfo }> {
    return this.run(async () => {
      this.handle = handle;
      const scanned = await this.scan(handle);
      this.baseline = { ...scanned.files };
      this.excludedPaths = scanned.excludedPaths;
      return scanned;
    });
  }

  refresh(): Promise<Record<string, BrowserWorkspaceFile> | null> {
    return this.run(async () => {
      if (!this.handle) return null;
      const scanned = await this.scan(this.handle);
      this.baseline = { ...scanned.files };
      this.excludedPaths = scanned.excludedPaths;
      return scanned.files;
    });
  }

  sync(files: Record<string, BrowserWorkspaceFile>): Promise<void> {
    return this.run(async () => {
      if (!this.handle) return;
      const previous = this.baseline;
      const syncableFiles = Object.fromEntries(
        Object.entries(files).filter(
          ([path, file]) => !this.isExcluded(path) && decodedFileSize(file) <= maximumFileBytes,
        ),
      );
      for (const [path, file] of Object.entries(syncableFiles)) {
        if (sameFile(previous[path], file)) continue;
        await this.write(this.handle, path, file);
      }
      for (const path of Object.keys(previous)) {
        if (path in syncableFiles) continue;
        await this.remove(this.handle, path);
      }
      this.baseline = syncableFiles;
    });
  }

  disconnect(): Promise<void> {
    return this.run(async () => {
      this.handle = null;
      this.baseline = {};
      this.excludedPaths.clear();
    });
  }

  private run<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.tail.then(operation, operation);
    this.tail = result.then(
      () => undefined,
      () => undefined,
    );
    return result;
  }

  private async scan(handle: FileSystemDirectoryHandle): Promise<{
    files: Record<string, BrowserWorkspaceFile>;
    info: BrowserDirectoryInfo;
    excludedPaths: Set<string>;
  }> {
    const files: Record<string, BrowserWorkspaceFile> = {};
    const excludedPaths = new Set<string>();
    let fileCount = 0;
    let skippedCount = 0;
    let totalBytes = 0;

    const walk = async (directory: FileSystemDirectoryHandle, prefix: string): Promise<void> => {
      for await (const [name, entry] of (directory as DirectoryHandle).entries()) {
        if (entry.kind === "directory") {
          if (ignoredDirectories.has(name)) {
            skippedCount++;
            continue;
          }
          await walk(entry as FileSystemDirectoryHandle, `${prefix}${name}/`);
          continue;
        }

        const file = await (entry as FileSystemFileHandle).getFile();
        if (
          fileCount >= maximumFiles ||
          file.size > maximumFileBytes ||
          totalBytes + file.size > maximumWorkspaceBytes
        ) {
          excludedPaths.add(`${workspaceRoot}${prefix}${name}`);
          skippedCount++;
          continue;
        }
        const bytes = new Uint8Array(await file.arrayBuffer());
        files[`${workspaceRoot}${prefix}${name}`] = encodeFile(bytes);
        fileCount++;
        totalBytes += file.size;
      }
    };

    await walk(handle, "");
    return { files, info: { name: handle.name, fileCount, skippedCount }, excludedPaths };
  }

  private isExcluded(path: string): boolean {
    if (this.excludedPaths.has(path)) return true;
    try {
      return relativePath(path).some((part) => ignoredDirectories.has(part));
    } catch {
      return true;
    }
  }

  private async write(
    root: FileSystemDirectoryHandle,
    path: string,
    file: BrowserWorkspaceFile,
  ): Promise<void> {
    const parts = relativePath(path);
    const directory = await parentDirectory(root, parts, true);
    const handle = await directory.getFileHandle(parts.at(-1) || "", { create: true });
    const writable = await handle.createWritable();
    try {
      await writable.write(decodeFile(file));
    } finally {
      await writable.close();
    }
  }

  private async remove(root: FileSystemDirectoryHandle, path: string): Promise<void> {
    const parts = relativePath(path);
    try {
      const directory = await parentDirectory(root, parts, false);
      await directory.removeEntry(parts.at(-1) || "");
    } catch (error) {
      if (error instanceof DOMException && error.name === "NotFoundError") return;
      throw error;
    }
  }
}
