"use strict";

const crypto = require("crypto");
const fs = require("fs");
const path = require("path");

const MAX_WHATSAPP_MEDIA_BYTES = 64 * 1024 * 1024;
const DEFAULT_MAX_JSON_BYTES = 1024 * 1024;
const DEFAULT_MAX_QUEUE_MESSAGES = 1000;
const DEFAULT_MAX_QUEUE_BYTES = 8 * 1024 * 1024;

function requireLoopbackHost(value) {
  const host = String(value || "").toLowerCase();
  if (!new Set(["127.0.0.1", "localhost", "::1"]).has(host)) {
    throw new Error("WHATSAPP_BRIDGE_HOST must be a loopback host");
  }
  return host;
}

function parsePort(value) {
  const port = Number(value);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error("WHATSAPP_BRIDGE_PORT must be an integer from 1 to 65535");
  }
  return port;
}

function clampMediaBytes(value) {
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed < 1) {
    return MAX_WHATSAPP_MEDIA_BYTES;
  }
  return Math.min(Math.floor(parsed), MAX_WHATSAPP_MEDIA_BYTES);
}

function positiveBound(value, fallback) {
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed < 1) {
    return fallback;
  }
  return parsed;
}

function clampedBound(value, fallback, maximum) {
  return Math.min(positiveBound(value, fallback), maximum);
}

function authorized(header, token) {
  const expected = Buffer.from(`Bearer ${token}`, "utf8");
  const actual = Buffer.from(String(header || ""), "utf8");
  return actual.length === expected.length && crypto.timingSafeEqual(actual, expected);
}

function containedRegularFile(root, value, maxBytes) {
  if (typeof value !== "string" || !value) {
    return null;
  }
  let resolvedRoot;
  let resolved;
  try {
    resolvedRoot = fs.realpathSync(root);
    resolved = fs.realpathSync(value);
    const relative = path.relative(resolvedRoot, resolved);
    if (!relative || relative === ".." || relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative)) {
      return null;
    }
    const stat = fs.statSync(resolved);
    if (!stat.isFile() || stat.size > maxBytes) {
      return null;
    }
  } catch (_error) {
    return null;
  }
  return resolved;
}

function readJson(req, maxBytes = DEFAULT_MAX_JSON_BYTES) {
  return new Promise((resolve, reject) => {
    const declared = Number(req.headers && req.headers["content-length"]);
    if (Number.isFinite(declared) && declared > maxBytes) {
      reject(Object.assign(new Error("request body is too large"), { statusCode: 413 }));
      return;
    }
    const chunks = [];
    let size = 0;
    let settled = false;
    const fail = (error) => {
      if (settled) return;
      settled = true;
      reject(error);
    };
    req.on("data", (chunk) => {
      if (settled) return;
      size += chunk.length;
      if (size > maxBytes) {
        fail(Object.assign(new Error("request body is too large"), { statusCode: 413 }));
        return;
      }
      chunks.push(chunk);
    });
    req.on("end", () => {
      if (settled) return;
      settled = true;
      if (chunks.length === 0) {
        resolve({});
        return;
      }
      try {
        resolve(JSON.parse(Buffer.concat(chunks).toString("utf8")));
      } catch (_error) {
        reject(Object.assign(new Error("request body must be valid JSON"), { statusCode: 400 }));
      }
    });
    req.on("error", fail);
  });
}

function decodedBase64Size(value) {
  const data = String(value || "");
  const padding = data.endsWith("==") ? 2 : data.endsWith("=") ? 1 : 0;
  return Math.floor((data.length * 3) / 4) - padding;
}

function enqueueBounded(queue, value, maxMessages = DEFAULT_MAX_QUEUE_MESSAGES) {
  if (queue.length >= maxMessages) {
    return false;
  }
  queue.push(value);
  return true;
}

module.exports = {
  DEFAULT_MAX_JSON_BYTES,
  DEFAULT_MAX_QUEUE_BYTES,
  DEFAULT_MAX_QUEUE_MESSAGES,
  MAX_WHATSAPP_MEDIA_BYTES,
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
};
