"use strict";

const assert = require("node:assert/strict");
const { EventEmitter } = require("node:events");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const security = require("./security.cjs");

test("loopback host and port validation reject exposed listeners", () => {
  for (const host of ["127.0.0.1", "localhost", "::1"]) {
    assert.equal(security.requireLoopbackHost(host), host);
  }
  assert.throws(() => security.requireLoopbackHost("0.0.0.0"), /loopback/);
  assert.throws(() => security.requireLoopbackHost("example.com"), /loopback/);
  assert.equal(security.parsePort("3000"), 3000);
  assert.throws(() => security.parsePort("0"), /1 to 65535/);
});

test("media limits always clamp to 64 MiB", () => {
  assert.equal(security.clampMediaBytes(undefined), 64 * 1024 * 1024);
  assert.equal(security.clampMediaBytes(12345), 12345);
  assert.equal(security.clampMediaBytes(65 * 1024 * 1024), 64 * 1024 * 1024);
  assert.equal(security.decodedBase64Size("dGVzdA=="), 4);
});

test("operator-configured bounds cannot disable memory ceilings", () => {
  assert.equal(security.clampedBound(undefined, 1024, 4096), 1024);
  assert.equal(security.clampedBound("2048", 1024, 4096), 2048);
  assert.equal(security.clampedBound("999999", 1024, 4096), 4096);
  assert.equal(security.clampedBound("invalid", 1024, 4096), 1024);
});

test("bearer authentication compares the complete value", () => {
  assert.equal(security.authorized("Bearer secret", "secret"), true);
  assert.equal(security.authorized("Bearer secre", "secret"), false);
  assert.equal(security.authorized("Basic secret", "secret"), false);
});

test("outbound files must be regular, bounded, and realpath-confined", () => {
  const parent = fs.mkdtempSync(path.join(os.tmpdir(), "dago-whatsapp-"));
  const root = path.join(parent, "media");
  fs.mkdirSync(root, { mode: 0o700 });
  const inside = path.join(root, "image.png");
  const outside = path.join(parent, "outside.png");
  fs.writeFileSync(inside, "image", { mode: 0o600 });
  fs.writeFileSync(outside, "secret", { mode: 0o600 });
  assert.equal(security.containedRegularFile(root, inside, 5), fs.realpathSync(inside));
  assert.equal(security.containedRegularFile(root, inside, 4), null);
  assert.equal(security.containedRegularFile(root, outside, 100), null);
  const link = path.join(root, "link.png");
  fs.symlinkSync(outside, link);
  assert.equal(security.containedRegularFile(root, link, 100), null);
  fs.rmSync(parent, { recursive: true, force: true });
});

test("queue refuses overflow without evicting accepted messages", () => {
  const queue = [];
  assert.equal(security.enqueueBounded(queue, "one", 1), true);
  assert.equal(security.enqueueBounded(queue, "two", 1), false);
  assert.deepEqual(queue, ["one"]);
});

test("JSON reader rejects declared and streamed oversized bodies", async () => {
  const declared = new EventEmitter();
  declared.headers = { "content-length": "9" };
  await assert.rejects(security.readJson(declared, 8), /too large/);

  const streamed = new EventEmitter();
  streamed.headers = {};
  const result = security.readJson(streamed, 8);
  streamed.emit("data", Buffer.from("123456789"));
  streamed.emit("end");
  await assert.rejects(result, /too large/);

  const valid = new EventEmitter();
  valid.headers = {};
  const parsed = security.readJson(valid, 32);
  valid.emit("data", Buffer.from('{"ok":true}'));
  valid.emit("end");
  assert.deepEqual(await parsed, { ok: true });
});
