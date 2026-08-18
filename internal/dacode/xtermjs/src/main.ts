import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import "./page.css";

declare global {
  interface Window {
    dacodeTerminal?: Terminal;
  }
}

async function copyText(text: string): Promise<"clipboard" | "exec-command" | "failed"> {
  if (!text) return "failed";
  try {
    await navigator.clipboard.writeText(text);
    return "clipboard";
  } catch {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.setAttribute("readonly", "");
    textarea.style.position = "fixed";
    textarea.style.opacity = "0";
    document.body.append(textarea);
    textarea.select();
    const copied = document.execCommand("copy");
    textarea.remove();
    return copied ? "exec-command" : "failed";
  }
}

type ClientMessage =
  | { type: "init"; cols: number; rows: number }
  | { type: "input"; data: string }
  | { type: "resize"; cols: number; rows: number }
  | { type: "redraw"; cols: number; rows: number };

const container = document.querySelector<HTMLElement>("#terminal");
const status = document.querySelector<HTMLElement>("#connection-status");
if (!container || !status) {
  throw new Error("terminal page is incomplete");
}

function openExternalURL(value: string, allowedProtocols: readonly string[]): void {
  if (value.length === 0 || value.length > 16 * 1024) {
    throw new Error("invalid URL payload size");
  }
  const target = new URL(value);
  if (!allowedProtocols.includes(target.protocol) || target.hostname === "" || target.username !== "" || target.password !== "") {
    throw new Error("URL is not a safe external target");
  }
  window.open(target.href, "_blank", "noopener,noreferrer");
  document.documentElement.dataset.openedUrl = target.href;
  document.documentElement.dataset.openUrlState = "opened";
}

const terminal = new Terminal({
  cursorBlink: false,
  cursorStyle: "bar",
  fontFamily: '"SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace',
  fontSize: 14,
  lineHeight: 1.1,
  linkHandler: {
    activate: (_event, value) => {
      try {
        openExternalURL(value, ["http:", "https:"]);
      } catch {
        document.documentElement.dataset.openUrlState = "rejected";
      }
    }
  },
  // The application owns transcript history and viewport position. Keeping a
  // second xterm scrollback buffer makes wheel gestures reveal stale screen
  // frames (including old composer drafts) instead of scrolling the chat.
  scrollback: 0,
  theme: {
    background: "#11121d",
    foreground: "#c0caf5",
    cursor: "#7aa2f7",
    cursorAccent: "#11121d",
    selectionBackground: "#33467c",
    black: "#11121d",
    red: "#f7768e",
    green: "#9ece6a",
    yellow: "#e0af68",
    blue: "#7aa2f7",
    magenta: "#bb9af7",
    cyan: "#7dcfff",
    white: "#c0caf5",
    brightBlack: "#545c7e",
    brightRed: "#ff899d",
    brightGreen: "#b9f27c",
    brightYellow: "#ffcf73",
    brightBlue: "#8db4ff",
    brightMagenta: "#c7a9ff",
    brightCyan: "#a4daff",
    brightWhite: "#ffffff"
  }
});
const fit = new FitAddon();
terminal.loadAddon(fit);
const initialBackground = terminal.options.theme?.background ?? "#11121d";
const clipboardHandler = terminal.parser.registerOscHandler(52, async (data) => {
  const separator = data.indexOf(";");
  const selector = separator >= 0 ? data.slice(0, separator) : "";
  const encoded = separator >= 0 ? data.slice(separator + 1) : "";
  if ((selector !== "c" && selector !== "") || encoded.length === 0 || encoded.length > 8 * 1024 * 1024) {
    document.documentElement.dataset.clipboardState = "rejected";
    return true;
  }
  try {
    const bytes = Uint8Array.from(atob(encoded), (character) => character.charCodeAt(0));
    const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    const method = await copyText(text);
    document.documentElement.dataset.clipboardState = method === "failed" ? "error" : "copied";
    document.documentElement.dataset.clipboardMethod = method;
  } catch {
    document.documentElement.dataset.clipboardState = "error";
  }
  return true;
});
const openURLHandler = terminal.parser.registerOscHandler(777, (data) => {
  const prefix = "dago-open-url;";
  if (!data.startsWith(prefix)) {
    document.documentElement.dataset.openUrlState = "rejected";
    return true;
  }
  try {
    const encoded = data.slice(prefix.length);
    if (encoded.length === 0 || encoded.length > 16 * 1024) {
      throw new Error("invalid URL payload size");
    }
    const bytes = Uint8Array.from(atob(encoded), (character) => character.charCodeAt(0));
    const value = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    openExternalURL(value, ["https:"]);
  } catch {
    document.documentElement.dataset.openUrlState = "rejected";
  }
  return true;
});
const backgroundHandler = terminal.parser.registerOscHandler(11, (data) => {
  if (!/^#[0-9a-fA-F]{6}$/.test(data)) return true;
  terminal.options.theme = { ...terminal.options.theme, background: data };
  document.documentElement.dataset.terminalBackground = data.toUpperCase();
  return true;
});
const backgroundResetHandler = terminal.parser.registerOscHandler(111, (data) => {
  if (data !== "") return true;
  terminal.options.theme = { ...terminal.options.theme, background: initialBackground };
  document.documentElement.dataset.terminalBackground = initialBackground.toUpperCase();
  return true;
});
terminal.open(container);
fit.fit();
terminal.focus();
window.dacodeTerminal = terminal;

const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
const socket = new WebSocket(`${protocol}//${window.location.host}/terminal`);
socket.binaryType = "arraybuffer";

function send(message: ClientMessage): void {
  if (socket.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify(message));
  }
}

socket.addEventListener("open", () => {
  send({ type: "init", cols: terminal.cols, rows: terminal.rows });
  status.textContent = "Connected";
  document.documentElement.dataset.terminalState = "connected";
});

socket.addEventListener("message", (event) => {
  if (typeof event.data === "string") {
    const message = JSON.parse(event.data) as { type: string; data?: string };
    if (message.type === "error") {
      terminal.writeln(`\r\nError: ${message.data ?? "terminal session failed"}`);
      status.textContent = "Error";
      document.documentElement.dataset.terminalState = "error";
    }
    return;
  }
  terminal.write(new Uint8Array(event.data as ArrayBuffer));
});

socket.addEventListener("close", () => {
  status.textContent = "Session ended";
  document.documentElement.dataset.terminalState = "closed";
});

socket.addEventListener("error", () => {
  status.textContent = "Connection failed";
  document.documentElement.dataset.terminalState = "error";
});

terminal.onData((data) => {
  if (data === "\u001b[32u") {
    send({ type: "input", data: " " });
    return;
  }
  // Some terminals do not advertise bracketed paste. A multi-character input
  // event is still atomic, while physical keypresses arrive one event at a time.
  // Mark the atomic run so the TUI can collapse and bind it as one paste.
  if (Array.from(data).length >= 3 && !data.includes("\u001b")) {
    send({ type: "input", data: `\u001b[200~${data}\u001b[201~` });
    return;
  }
  send({ type: "input", data });
});
container.addEventListener("wheel", (event) => {
  event.preventDefault();
  event.stopImmediatePropagation();
  terminal.focus();
  const direction = Math.sign(event.deltaY);
  if (direction === 0) return;
  send({ type: "input", data: direction < 0 ? "\u001f" : "\u001e" });
}, { capture: true, passive: false });
let lastBackslashAt = Number.NEGATIVE_INFINITY;
terminal.attachCustomKeyEventHandler((event) => {
  if (["CapsLock", "NumLock", "ScrollLock"].includes(event.key)) {
    event.preventDefault();
    return false;
  }
  if (event.type === "keydown" && event.key === "\\" && !event.ctrlKey && !event.altKey && !event.metaKey) {
    lastBackslashAt = performance.now();
    return true;
  }
  if (event.type === "keydown" && event.key === "Enter" && performance.now() - lastBackslashAt <= 150) {
    lastBackslashAt = 0;
    event.preventDefault();
    send({ type: "input", data: "\u007f\n" });
    return false;
  }
  if (event.key === "Enter" && (event.shiftKey || event.altKey || event.ctrlKey) && !event.metaKey) {
    event.preventDefault();
    if (event.type === "keydown") send({ type: "input", data: "\n" });
    return false;
  }
  if (event.key === "Backspace" && (event.ctrlKey || event.altKey) && !event.metaKey) {
    event.preventDefault();
    if (event.type === "keydown") send({ type: "input", data: "\u0017" });
    return false;
  }
  if (event.key.toLowerCase() === "c" && event.ctrlKey && event.shiftKey && !event.altKey && !event.metaKey) {
    event.preventDefault();
    if (event.type === "keydown") {
      void copyText(terminal.getSelection()).then((method) => {
        document.documentElement.dataset.selectionCopyMethod = method;
        document.documentElement.dataset.clipboardState = method === "failed" ? "error" : "copied";
      });
    }
    return false;
  }
  if (event.ctrlKey && !event.altKey && !event.metaKey && event.key.toLowerCase() === "j") {
    event.preventDefault();
    if (event.type === "keydown") send({ type: "input", data: "\n" });
    return false;
  }
  return true;
});

let refocusedAt = Number.NEGATIVE_INFINITY;
window.addEventListener("focus", () => {
  refocusedAt = performance.now();
  terminal.focus();
});
container.addEventListener(
  "pointerdown",
  (event) => {
    if (performance.now() - refocusedAt > 300) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    document.documentElement.dataset.refocusClick = "suppressed";
    terminal.focus();
  },
  { capture: true }
);
container.addEventListener("dragover", (event) => event.preventDefault());
container.addEventListener("drop", (event) => {
  event.preventDefault();
  const transfer = event.dataTransfer;
  const value = transfer?.getData("text/uri-list") || transfer?.getData("text/plain") || "";
  if (value) terminal.paste(value);
});

let resizeFrame = 0;
let redrawTimer = 0;
const synchronizeTerminalSize = (cols: number, rows: number) => {
  if (socket.readyState !== WebSocket.OPEN) return;
  send({ type: "resize", cols, rows });
  window.clearTimeout(redrawTimer);
  redrawTimer = window.setTimeout(() => {
    terminal.clear();
    terminal.write("\u001b[2J\u001b[H");
    send({ type: "redraw", cols, rows });
  }, 40);
};
const terminalResizeHandler = terminal.onResize(({ cols, rows }) => synchronizeTerminalSize(cols, rows));
const resizeObserver = new ResizeObserver(() => {
  cancelAnimationFrame(resizeFrame);
  resizeFrame = requestAnimationFrame(() => {
    fit.fit();
  });
});
resizeObserver.observe(container);

window.addEventListener("beforeunload", () => {
  resizeObserver.disconnect();
  window.clearTimeout(redrawTimer);
  socket.close();
  openURLHandler.dispose();
  clipboardHandler.dispose();
  backgroundHandler.dispose();
  backgroundResetHandler.dispose();
  terminalResizeHandler.dispose();
  terminal.dispose();
});
