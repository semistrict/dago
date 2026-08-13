import {
  Bash,
  InMemoryFs,
  MountableFs,
  type IFileSystem,
} from "just-bash/browser";

export type ShellRequest = {
  command: string;
  cwd: string;
  timeout_milliseconds: number;
};

export type ShellResponse = {
  stdout: string;
  stderr: string;
  exit_code: number;
};

export type JustBashRuntimeOptions = {
  filesystem: IFileSystem;
  mountPoint?: string;
};

// JustBashRuntime keeps one shell instance mounted on the same filesystem used
// by Go agent tools. Commands exchange output only; file bodies never cross the
// shell bridge.
export class JustBashRuntime {
  private readonly shell: Bash;
  private readonly mountPoint: string;

  constructor(options: JustBashRuntimeOptions) {
    this.mountPoint = options.mountPoint ?? "/workspace";
    const filesystem = new MountableFs({
      base: new InMemoryFs(),
      mounts: [{ mountPoint: this.mountPoint, filesystem: options.filesystem }],
    });
    this.shell = new Bash({ fs: filesystem, cwd: this.mountPoint });
  }

  async execute(request: ShellRequest): Promise<ShellResponse> {
    const abort = new AbortController();
    const timer =
      request.timeout_milliseconds > 0
        ? setTimeout(() => abort.abort(), request.timeout_milliseconds)
        : undefined;
    try {
      const result = await this.shell.exec(request.command, {
        cwd: request.cwd || this.mountPoint,
        signal: abort.signal,
      });
      return {
        stdout: result.stdout,
        stderr: result.stderr,
        exit_code: result.exitCode,
      };
    } catch (error) {
      if (!abort.signal.aborted) throw error;
      return {
        stdout: "",
        stderr: `Command timed out after ${request.timeout_milliseconds}ms`,
        exit_code: 124,
      };
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
  }

  async executeJSON(encoded: string): Promise<string> {
    return JSON.stringify(
      await this.execute(JSON.parse(encoded) as ShellRequest),
    );
  }
}
