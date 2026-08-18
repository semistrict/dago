"use strict";

const fs = require("fs");
const http = require("http");
const path = require("path");
const { Client, LocalAuth, MessageMedia } = require("whatsapp-web.js");
const qrcode = require("qrcode-terminal");
const {
  DEFAULT_MAX_JSON_BYTES,
  DEFAULT_MAX_QUEUE_BYTES,
  DEFAULT_MAX_QUEUE_MESSAGES,
  authorized,
  clampMediaBytes,
  clampedBound,
  containedRegularFile,
  decodedBase64Size,
  enqueueBounded,
  parsePort,
  positiveBound,
  readJson,
  requireLoopbackHost,
} = require("./security.cjs");

const host = requireLoopbackHost(process.env.WHATSAPP_BRIDGE_HOST || "127.0.0.1");
const port = parsePort(process.env.WHATSAPP_BRIDGE_PORT || "3000");
const sessionDir = path.resolve(process.env.WHATSAPP_SESSION_DIR || path.join(process.cwd(), ".whatsapp"));
const mediaDir = path.resolve(process.env.WHATSAPP_MEDIA_DIR || path.join(sessionDir, "..", "media"));
const botHeader = String(process.env.WHATSAPP_BOT_HEADER || "deepagents bot").slice(0, 256);
const bridgeToken = String(process.env.WHATSAPP_BRIDGE_TOKEN || "");
const maxMediaBytes = clampMediaBytes(process.env.WHATSAPP_MAX_MEDIA_BYTES);
const maxJsonBytes = clampedBound(process.env.WHATSAPP_MAX_JSON_BYTES, DEFAULT_MAX_JSON_BYTES, 8 * 1024 * 1024);
const maxQueueMessages = clampedBound(
  process.env.WHATSAPP_MAX_QUEUE_MESSAGES,
  DEFAULT_MAX_QUEUE_MESSAGES,
  10_000,
);
const maxQueueBytes = clampedBound(
  process.env.WHATSAPP_MAX_QUEUE_BYTES,
  DEFAULT_MAX_QUEUE_BYTES,
  64 * 1024 * 1024,
);
const webVersionCacheUrl =
  process.env.WHATSAPP_WEB_VERSION_CACHE_URL ||
  "https://raw.githubusercontent.com/wppconnect-team/wa-version/main/html/2.3000.1026029003.html";

if (!bridgeToken || bridgeToken.length > 4096) {
  console.error("WHATSAPP_BRIDGE_TOKEN is required and must be at most 4096 bytes");
  process.exit(1);
}

fs.mkdirSync(sessionDir, { recursive: true, mode: 0o700 });
fs.mkdirSync(mediaDir, { recursive: true, mode: 0o700 });
fs.chmodSync(sessionDir, 0o700);
fs.chmodSync(mediaDir, 0o700);
cleanStaleLocks(sessionDir);

const chromePath = process.env.CHROME_PATH || process.env.WHATSAPP_CHROME_PATH || findChrome();
const puppeteer = {
  headless: true,
  args: ["--no-sandbox", "--disable-setuid-sandbox", "--disable-gpu"],
};
if (chromePath) puppeteer.executablePath = chromePath;

const client = new Client({
  authStrategy: new LocalAuth({ dataPath: sessionDir }),
  puppeteer,
  webVersionCache: { type: "remote", remotePath: webVersionCacheUrl },
});

let status = "disconnected";
let botId = null;
const queue = [];
let queueBytes = 0;
const sentMessages = new Map();
const recentBodies = new Map();
const MAX_SENT_MESSAGES = 200;
const SENT_BODY_TTL_MS = 5 * 60 * 1000;

process.on("unhandledRejection", (reason) => {
  console.error("Unhandled rejection:", reason && reason.message ? reason.message : String(reason));
});

client.on("qr", (qr) => {
  status = "qr_pending";
  console.log("Scan this QR code to pair WhatsApp:");
  qrcode.generate(qr, { small: true });
});
client.on("ready", () => {
  status = "connected";
  botId = client.info && client.info.wid ? client.info.wid._serialized : null;
  console.log(`WhatsApp connected as ${botId || "unknown"}`);
});
client.on("disconnected", (reason) => {
  status = "disconnected";
  console.log(`WhatsApp disconnected: ${reason || "unknown reason"}`);
});
client.on("auth_failure", (message) => {
  status = "disconnected";
  console.error(`WhatsApp auth failure: ${message || "unknown error"}`);
});
client.on("message_create", (message) => {
  void enqueueMessage(message);
});

async function enqueueMessage(message) {
  if (message.from === "status@broadcast" || isBridgeSentMessage(message)) return;
  const fromSelf = isSelfMessage(message);
  const chat = await safely(() => message.getChat());
  const contact = await safely(() => message.getContact());
  const messageId = serializedId(message.id);
  const chatId = serializedId(chat && chat.id) || (fromSelf ? message.to : message.from);
  if (!messageId || !chatId) return;
  const media = await downloadMedia(message, messageId);
  const mimeType = media.length > 0 ? String(media[0].mimeType || "") : messageMimeType(message);
  const mediaType = classifyMedia(message.type, mimeType, Boolean(message.hasMedia));
  const senderId = message.author || (fromSelf && botId ? botId : message.from);
  const isGroup = Boolean(chat && chat.isGroup) || String(chatId).endsWith("@g.us");
  const entry = {
    body: String(message.body || "").slice(0, 1024 * 1024),
    chatId,
    chatIdFrom: message.from,
    chatName: (chat && chat.name) || chatId,
    chatType: isGroup ? "group" : "direct",
    senderId: senderId || null,
    senderName: (contact && (contact.pushname || contact.name || contact.shortName)) || senderId || null,
    messageId,
    messageType: message.type || "chat",
    mediaType,
    hasMedia: Boolean(message.hasMedia || media.length > 0),
    mediaPaths: media.map((item) => item.path),
    mediaMimeTypes: media.map((item) => item.mimeType),
    fromSelf,
    raw_message: {
      from: message.from,
      to: message.to,
      author: message.author || null,
      fromMe: Boolean(message.fromMe),
      timestamp: message.timestamp || null,
    },
  };
  const entryBytes = Buffer.byteLength(JSON.stringify(entry), "utf8");
  if (entryBytes > maxQueueBytes || queueBytes > maxQueueBytes - entryBytes || !enqueueBounded(queue, entry, maxQueueMessages)) {
    console.error(`[bridge] Dropping message ${messageId}: inbound queue is full`);
  } else {
    queueBytes += entryBytes;
  }
}

async function downloadMedia(message, messageId) {
  if (!message.hasMedia) return [];
  const expected = Number(message && message._data && (message._data.size || message._data.fileSize));
  if (Number.isFinite(expected) && expected > maxMediaBytes) return [];
  try {
    const media = await message.downloadMedia();
    if (!media || !media.data || decodedBase64Size(media.data) > maxMediaBytes) return [];
    const extension = mediaExtension(media.mimetype, message.type);
    const safeID = messageId.replace(/[^A-Za-z0-9]/g, "_").slice(0, 160);
    const filePath = path.join(mediaDir, `${Date.now()}_${safeID}.${extension}`);
    fs.writeFileSync(filePath, Buffer.from(media.data, "base64"), { mode: 0o600, flag: "wx" });
    return [{ path: filePath, mimeType: media.mimetype || "application/octet-stream" }];
  } catch (error) {
    console.error("Media download failed:", error && error.message ? error.message : String(error));
    return [];
  }
}

async function handle(req, res) {
  try {
    if (!authorized(req.headers.authorization, bridgeToken)) {
      sendJson(res, 401, { success: false, error: "unauthorized" });
      return;
    }
    if (req.method === "GET" && req.url === "/health") {
      sendJson(res, 200, { status, botId });
      return;
    }
    if (req.method === "GET" && req.url === "/messages") {
      const messages = queue.splice(0, queue.length);
      queueBytes = 0;
      sendJson(res, 200, messages);
      return;
    }
    if (req.method === "POST" && req.url === "/send") {
      const body = await readJson(req, maxJsonBytes);
      const chatId = body.chatId || body.chat_id;
      const text = body.text || body.message || "";
      if (typeof chatId !== "string" || !chatId || chatId.length > 1024 || typeof text !== "string" || !text || [...text].length > 4096) {
        sendJson(res, 400, { success: false, error: "valid chat_id and text required" });
        return;
      }
      rememberBody(text);
      const sent = await client.sendMessage(chatId, text, { quotedMessageId: body.replyTo || body.reply_to || undefined });
      rememberSent(sent, text);
      sendMessageResult(res, sent);
      return;
    }
    if (req.method === "POST" && req.url === "/send-media") {
      const body = await readJson(req, maxJsonBytes);
      const chatId = body.chatId || body.chat_id;
      const filePath = body.filePath || body.path;
      const safePath = containedRegularFile(mediaDir, filePath, maxMediaBytes);
      if (typeof chatId !== "string" || !chatId || chatId.length > 1024 || !safePath) {
        sendJson(res, 400, { success: false, error: "valid chat_id and confined media path required" });
        return;
      }
      const media = MessageMedia.fromFilePath(safePath);
      const caption = typeof body.caption === "string" ? body.caption.slice(0, 4096) : undefined;
      if (caption) rememberBody(caption);
      const sent = await client.sendMessage(chatId, media, {
        caption,
        sendMediaAsDocument: body.mediaType === "document",
      });
      rememberSent(sent, caption || "");
      sendMessageResult(res, sent);
      return;
    }
    if (req.method === "POST" && req.url === "/typing") {
      const body = await readJson(req, maxJsonBytes);
      const chatId = body.chatId || body.chat_id;
      if (typeof chatId !== "string" || !chatId || chatId.length > 1024) {
        sendJson(res, 400, { success: false, error: "chat_id required" });
        return;
      }
      const chat = await client.getChatById(chatId);
      await chat.sendStateTyping();
      sendJson(res, 200, { success: true });
      return;
    }
    if (req.method === "POST" && req.url === "/edit") {
      const body = await readJson(req, maxJsonBytes);
      const messageId = body.messageId || body.message_id;
      const content = body.content || body.message || "";
      if (typeof messageId !== "string" || !messageId || messageId.length > 1024 || typeof content !== "string" || !content || [...content].length > 4096) {
        sendJson(res, 400, { success: false, error: "valid message_id and content required" });
        return;
      }
      const message = sentMessages.get(messageId) || (await client.getMessageById(messageId));
      if (!message) {
        sendJson(res, 200, { success: false, error: "message not found" });
        return;
      }
      rememberBody(content);
      const edited = await message.edit(content);
      rememberSent(edited, content);
      sendMessageResult(res, edited, messageId);
      return;
    }
    sendJson(res, 404, { success: false, error: "not found" });
  } catch (error) {
    const statusCode = Number(error && error.statusCode) || 500;
    const detail = error && error.message ? error.message : "bridge request failed";
    sendJson(res, statusCode, { success: false, error: String(detail).slice(0, 4096) });
  }
}

function sendJson(res, code, body) {
  const data = Buffer.from(JSON.stringify(body));
  res.writeHead(code, { "content-type": "application/json", "content-length": String(data.length), "x-content-type-options": "nosniff" });
  res.end(data);
}

function sendMessageResult(res, message, fallback = null) {
  const messageId = serializedId(message && message.id) || fallback;
  sendJson(res, 200, { success: true, message_id: messageId, messageId });
}

function serializedId(value) {
  return value && value._serialized ? value._serialized : null;
}

function isSelfMessage(message) {
  return Boolean(message.fromMe || (message.id && message.id.fromMe) || String(serializedId(message.id) || "").startsWith("true_"));
}

function isBridgeSentMessage(message) {
  const id = serializedId(message.id);
  const body = String(message.body || "");
  return Boolean((id && sentMessages.has(id)) || recentBodies.has(body) || (body && body.startsWith(`*${botHeader}*`)));
}

function rememberSent(message, body) {
  const id = serializedId(message && message.id);
  if (!id) return;
  sentMessages.set(id, message);
  rememberBody(body);
  if (sentMessages.size > MAX_SENT_MESSAGES) sentMessages.delete(sentMessages.keys().next().value);
}

function rememberBody(body) {
  if (!body) return;
  const value = String(body);
  recentBodies.set(value, (recentBodies.get(value) || 0) + 1);
  setTimeout(() => {
    const count = recentBodies.get(value) || 0;
    if (count <= 1) recentBodies.delete(value);
    else recentBodies.set(value, count - 1);
  }, SENT_BODY_TTL_MS).unref();
}

function classifyMedia(type, mimeType, hasMedia) {
  const raw = String(type || "").toLowerCase();
  const mimeValue = String(mimeType || "").toLowerCase();
  if (raw === "ptt" || raw === "audio" || mimeValue.startsWith("audio/")) return "voice";
  if (raw === "image" || raw === "sticker" || mimeValue.startsWith("image/")) return "image";
  if (raw === "video" || mimeValue.startsWith("video/")) return "video";
  return hasMedia ? "document" : "text";
}

function messageMimeType(message) {
  const data = message && message._data ? message._data : {};
  return String(message.mimetype || data.mimetype || data.mimetypeOverride || "");
}

function mediaExtension(mimeType, messageType) {
  const subtype = String(mimeType || "").split(";", 1)[0].split("/")[1] || "";
  const cleaned = subtype.replace(/[^A-Za-z0-9]/g, "");
  if (cleaned) return cleaned === "plain" ? "txt" : cleaned;
  return messageType === "ptt" || messageType === "audio" ? "ogg" : "bin";
}

async function safely(call) {
  try {
    return await call();
  } catch (_error) {
    return null;
  }
}

function cleanStaleLocks(directory) {
  let entries;
  try {
    entries = fs.readdirSync(directory, { withFileTypes: true });
  } catch (_error) {
    return;
  }
  for (const entry of entries) {
    const full = path.join(directory, entry.name);
    if (entry.isDirectory()) cleanStaleLocks(full);
    else if (/^Singleton(Lock|Socket|Cookie)$/.test(entry.name)) {
      try { fs.unlinkSync(full); } catch (_error) { /* best effort */ }
    }
  }
}

function findChrome() {
  const candidates = process.platform === "darwin"
    ? ["/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium"]
    : ["/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium", "/usr/bin/chromium-browser"];
  return candidates.find((candidate) => fs.existsSync(candidate));
}

const server = http.createServer((req, res) => { void handle(req, res); });
server.listen(port, host, () => console.log(`WhatsApp bridge listening on http://${host}:${port}`));
client.initialize();

process.on("SIGTERM", async () => {
  server.close();
  try { await client.destroy(); } catch (_error) { /* exiting */ }
  process.exit(0);
});
