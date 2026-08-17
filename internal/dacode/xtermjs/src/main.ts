import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import "./page.css";

declare global {
  interface Window {
    dacodeTerminal?: Terminal;
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

const terminal = new Terminal({
  cursorBlink: false,
  cursorStyle: "bar",
  fontFamily: '"SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace',
  fontSize: 14,
  lineHeight: 1.1,
  scrollback: 4_000,
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

terminal.onData((data) => send({ type: "input", data }));
container.addEventListener("wheel", (event) => {
  event.preventDefault();
  event.stopImmediatePropagation();
  terminal.focus();
  const direction = Math.sign(event.deltaY);
  if (direction === 0) return;
  send({ type: "input", data: direction < 0 ? "\u001f" : "\u001e" });
}, { capture: true, passive: false });
terminal.attachCustomKeyEventHandler((event) => {
  if (event.ctrlKey && !event.altKey && !event.metaKey && event.key.toLowerCase() === "j") {
    event.preventDefault();
    if (event.type === "keydown") send({ type: "input", data: "\n" });
    return false;
  }
  return true;
});

let resizeFrame = 0;
let redrawTimer = 0;
const resizeObserver = new ResizeObserver(() => {
  cancelAnimationFrame(resizeFrame);
  window.clearTimeout(redrawTimer);
  resizeFrame = requestAnimationFrame(() => {
    fit.fit();
    if (socket.readyState !== WebSocket.OPEN) return;
    send({ type: "resize", cols: terminal.cols, rows: terminal.rows });
    redrawTimer = window.setTimeout(() => {
      terminal.clear();
      terminal.write("\u001b[2J\u001b[H");
      send({ type: "redraw", cols: terminal.cols, rows: terminal.rows });
    }, 40);
  });
});
resizeObserver.observe(container);

window.addEventListener("beforeunload", () => {
  resizeObserver.disconnect();
  window.clearTimeout(redrawTimer);
  socket.close();
  terminal.dispose();
});
