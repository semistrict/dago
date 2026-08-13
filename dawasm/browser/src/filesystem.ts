import type {
  BufferEncoding,
  CpOptions,
  FileContent,
  FsStat,
  IFileSystem,
  MkdirOptions,
  RmOptions,
} from "just-bash/browser";

export type BrowserDirectoryInfo = {
  name: string;
  fileCount: number;
  skippedCount: number;
};

type ReadOptions = { encoding?: BufferEncoding | null } | BufferEncoding;
type WriteOptions = { encoding?: BufferEncoding } | BufferEncoding;

export type WasmFileSystemAdapterOptions = {
  execute: (operation: string, payload: string) => Promise<string>;
  paths: () => string;
  workspaceRoot?: string;
};

function normalizePath(value: string): string {
  const parts: string[] = [];
  for (const part of value.split("\\").join("/").split("/")) {
    if (!part || part === ".") continue;
    if (part === "..") parts.pop();
    else parts.push(part);
  }
  return `/${parts.join("/")}`;
}

function workspacePath(value: string, root: string): string {
  const normalized = normalizePath(value);
  return normalized === "/" ? root : `${root}${normalized}`;
}

function mountedPath(value: string, root: string): string {
  if (value === root) return "/";
  return value.startsWith(`${root}/`) ? value.slice(root.length) : value;
}

function encoding(
  options?: ReadOptions | WriteOptions,
): BufferEncoding | null | undefined {
  return typeof options === "string" ? options : options?.encoding;
}

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
  for (let index = 0; index < decoded.length; index++)
    bytes[index] = decoded.charCodeAt(index);
  return bytes;
}

function encodeContent(
  content: FileContent,
  options?: WriteOptions,
): Uint8Array {
  if (content instanceof Uint8Array) return new Uint8Array(content);
  const selected = encoding(options) || "utf8";
  if (selected === "base64") return base64ToBytes(content);
  if (selected === "hex") {
    if (content.length % 2 !== 0 || !/^[0-9a-f]*$/i.test(content))
      throw new Error("EINVAL: invalid hex content");
    return Uint8Array.from({ length: content.length / 2 }, (_, index) =>
      Number.parseInt(content.slice(index * 2, index * 2 + 2), 16),
    );
  }
  if (selected === "binary" || selected === "latin1") {
    return Uint8Array.from(
      content,
      (character) => character.charCodeAt(0) & 0xff,
    );
  }
  if (selected === "ascii") {
    return Uint8Array.from(
      content,
      (character) => character.charCodeAt(0) & 0x7f,
    );
  }
  return new TextEncoder().encode(content);
}

function decodeContent(bytes: Uint8Array, options?: ReadOptions): string {
  const selected = encoding(options) || "utf8";
  if (selected === "base64") return bytesToBase64(bytes);
  if (selected === "hex")
    return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join(
      "",
    );
  if (selected === "binary" || selected === "latin1") {
    let result = "";
    for (let index = 0; index < bytes.length; index += 0x8000) {
      result += String.fromCharCode(...bytes.subarray(index, index + 0x8000));
    }
    return result;
  }
  if (selected === "ascii")
    return String.fromCharCode(...bytes.map((byte) => byte & 0x7f));
  return new TextDecoder().decode(bytes);
}

export class WasmFileSystemAdapter implements IFileSystem {
  private readonly workspaceRoot: string;

  constructor(private readonly options: WasmFileSystemAdapterOptions) {
    this.workspaceRoot = normalizePath(options.workspaceRoot || "/workspace");
  }

  private async invoke<T>(
    operation: string,
    payload: Record<string, unknown>,
  ): Promise<T> {
    const encoded = await this.options.execute(
      operation,
      JSON.stringify(payload),
    );
    return (encoded ? JSON.parse(encoded) : undefined) as T;
  }

  private workspacePath(value: string): string {
    return workspacePath(value, this.workspaceRoot);
  }

  private mountedPath(value: string): string {
    return mountedPath(value, this.workspaceRoot);
  }

  async readFile(path: string, options?: ReadOptions): Promise<string> {
    return decodeContent(await this.readFileBuffer(path), options);
  }

  async readFileBuffer(path: string): Promise<Uint8Array> {
    const result = await this.invoke<{ content: string }>("read_file", {
      path: this.workspacePath(path),
    });
    return base64ToBytes(result.content);
  }

  async writeFile(
    path: string,
    content: FileContent,
    options?: WriteOptions,
  ): Promise<void> {
    await this.invoke("write_file", {
      path: this.workspacePath(path),
      content: bytesToBase64(encodeContent(content, options)),
    });
  }

  async appendFile(
    path: string,
    content: FileContent,
    options?: WriteOptions,
  ): Promise<void> {
    await this.invoke("append_file", {
      path: this.workspacePath(path),
      content: bytesToBase64(encodeContent(content, options)),
    });
  }

  exists(path: string): Promise<boolean> {
    return this.invoke("exists", { path: this.workspacePath(path) });
  }

  async stat(path: string): Promise<FsStat> {
    const value = await this.invoke<Record<string, unknown>>("stat", {
      path: this.workspacePath(path),
    });
    return this.decodeStat(value);
  }

  async lstat(path: string): Promise<FsStat> {
    const value = await this.invoke<Record<string, unknown>>("lstat", {
      path: this.workspacePath(path),
    });
    return this.decodeStat(value);
  }

  mkdir(path: string, options?: MkdirOptions): Promise<void> {
    return this.invoke("mkdir", {
      path: this.workspacePath(path),
      recursive: options?.recursive,
    });
  }

  async readdir(path: string): Promise<string[]> {
    const entries = await this.readdirWithFileTypes(path);
    return entries.map((entry) => entry.name);
  }

  async readdirWithFileTypes(path: string): Promise<
    Array<{
      name: string;
      isFile: boolean;
      isDirectory: boolean;
      isSymbolicLink: boolean;
    }>
  > {
    const entries = await this.invoke<
      Array<{ path: string; is_dir?: boolean }>
    >("readdir", {
      path: this.workspacePath(path),
    });
    return entries.map((entry) => ({
      name: entry.path.replace(/\/$/, "").split("/").at(-1) || "",
      isFile: !entry.is_dir,
      isDirectory: Boolean(entry.is_dir),
      isSymbolicLink: false,
    }));
  }

  rm(path: string, options?: RmOptions): Promise<void> {
    return this.invoke("rm", {
      path: this.workspacePath(path),
      recursive: options?.recursive,
      force: options?.force,
    });
  }

  cp(source: string, destination: string, options?: CpOptions): Promise<void> {
    return this.invoke("cp", {
      source: this.workspacePath(source),
      destination: this.workspacePath(destination),
      recursive: options?.recursive,
    });
  }

  mv(source: string, destination: string): Promise<void> {
    return this.invoke("mv", {
      source: this.workspacePath(source),
      destination: this.workspacePath(destination),
    });
  }

  resolvePath(base: string, value: string): string {
    return value.startsWith("/")
      ? normalizePath(value)
      : normalizePath(`${base}/${value}`);
  }

  getAllPaths(): string[] {
    return (JSON.parse(this.options.paths()) as string[]).map((value) =>
      this.mountedPath(value),
    );
  }

  chmod(path: string, mode: number): Promise<void> {
    return this.invoke("chmod", { path: this.workspacePath(path), mode });
  }

  symlink(target: string, linkPath: string): Promise<void> {
    return this.invoke("symlink", {
      source: this.workspacePath(target),
      destination: this.workspacePath(linkPath),
    });
  }

  link(existingPath: string, newPath: string): Promise<void> {
    return this.invoke("link", {
      source: this.workspacePath(existingPath),
      destination: this.workspacePath(newPath),
    });
  }

  readlink(path: string): Promise<string> {
    return this.invoke("readlink", { path: this.workspacePath(path) });
  }

  async realpath(path: string): Promise<string> {
    return this.mountedPath(
      await this.invoke("realpath", { path: this.workspacePath(path) }),
    );
  }

  utimes(path: string, _atime: Date, mtime: Date): Promise<void> {
    return this.invoke("utimes", {
      path: this.workspacePath(path),
      mtime: mtime.toISOString(),
    });
  }

  private decodeStat(value: Record<string, unknown>): FsStat {
    return {
      isFile: Boolean(value.is_file),
      isDirectory: Boolean(value.is_directory),
      isSymbolicLink: Boolean(value.is_symbolic_link),
      mode: Number(value.mode),
      size: Number(value.size),
      mtime: new Date(String(value.mtime)),
      identity: String(value.identity),
    };
  }
}
