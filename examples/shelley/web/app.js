const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

const state = {
  status: null,
  conversations: [],
  current: null,
  messages: [],
  attachments: [],
  running: false,
  activePanel: null,
  filePath: "/",
  editorPath: "",
  pendingApproval: null,
};

let oauthPollTimer = null;

async function api(url, options = {}) {
  const response = await fetch(url, options);
  if (response.status === 204) return null;
  const value = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(value.error || `${response.status} ${response.statusText}`);
  return value;
}

function jsonOptions(method, body) {
  return { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) };
}

function toast(message, kind = "info") {
  const item = document.createElement("div");
  item.className = `toast ${kind}`;
  item.textContent = message;
  $("#toast-region").append(item);
  setTimeout(() => item.remove(), 4800);
}

function relativeTime(value) {
  const seconds = Math.max(0, Math.round((Date.now() - new Date(value).getTime()) / 1000));
  if (seconds < 60) return "NOW";
  if (seconds < 3600) return `${Math.floor(seconds / 60)}M AGO`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}H AGO`;
  return `${Math.floor(seconds / 86400)}D AGO`;
}

function setRunning(running) {
  state.running = running;
  const waiting = !running && Boolean(state.pendingApproval?.length);
  const blocked = running || waiting;
  document.body.classList.toggle("running", running);
  $("#run-state").textContent = running ? "RUNNING" : waiting ? "WAITING" : "IDLE";
  $("#run-state").classList.toggle("running", running);
  $("#run-state").classList.toggle("waiting", waiting);
  $("#run-state").disabled = !waiting;
  $("#prompt").disabled = blocked;
  $("#send-button").disabled = blocked;
  renderApprovalNotice(waiting ? state.pendingApproval : []);
}

function approvalSummary(request) {
  const call = request?.call || {};
  const args = call.arguments;
  if (call.name === "execute" && args && typeof args === "object" && args.command) return args.command;
  const encoded = typeof args === "string" ? args : JSON.stringify(args || {});
  return `${call.name || "action"} ${encoded}`.trim();
}

function renderApprovalNotice(requests = []) {
  const notice = $("#approval-notice");
  const visible = requests.length > 0;
  notice.classList.toggle("hidden", !visible);
  if (!visible) return;
  const count = requests.length;
  $("#approval-notice-title").textContent = count === 1
    ? `${requests[0].call?.name || "An action"} is waiting for your approval`
    : `${count} actions are waiting for your approval`;
  $("#approval-notice-summary").textContent = requests.map(approvalSummary).join(" · ");
}

async function refreshStatus() {
  state.status = await api("/api/status");
  const ready = state.status.ready;
  $("#auth-dot").classList.toggle("ready", ready);
  $("#auth-label").textContent = ready ? `${state.status.auth_mode.replace("_", " ")} · ${state.status.settings.model}` : "Setup required";
  $("#settings-model").value = state.status.settings.model;
  $("#settings-sandbox").value = state.status.settings.sandbox_name || "";
  $$('input[name="backend"]').forEach(input => input.checked = input.value === state.status.settings.backend);
  $("#workspace-label").textContent = state.status.workspace;
  if (state.status.oauth_state === "complete") $("#oauth-copy").textContent = "Subscription sign-in is complete.";
  if (state.status.oauth_state === "failed") $("#oauth-copy").textContent = state.status.oauth_error;
}

async function refreshConversations(query = "") {
  state.conversations = await api(`/api/conversations?q=${encodeURIComponent(query)}`);
  renderConversationList();
}

function renderConversationList() {
  const list = $("#conversation-list");
  list.replaceChildren();
  if (!state.conversations.length) {
    const empty = document.createElement("p");
    empty.className = "muted";
    empty.style.padding = "14px";
    empty.textContent = "No expeditions yet.";
    list.append(empty);
    return;
  }
  state.conversations.forEach(conversation => {
    const button = document.createElement("button");
    button.className = `conversation-item ${state.current?.id === conversation.id ? "active" : ""}`;
    button.dataset.id = conversation.id;
    const title = document.createElement("strong");
    title.textContent = conversation.title;
    const remove = document.createElement("button");
    remove.className = "delete-chat";
    remove.type = "button";
    remove.title = "Delete conversation";
    remove.textContent = "×";
    remove.addEventListener("click", event => { event.stopPropagation(); deleteConversation(conversation); });
    const meta = document.createElement("small");
    meta.textContent = `${relativeTime(conversation.updated_at)} · ${conversation.backend.toUpperCase()}`;
    button.append(title, remove, meta);
    button.addEventListener("click", () => selectConversation(conversation.id));
    list.append(button);
  });
}

async function newConversation(title = "") {
  const value = await api("/api/conversations", jsonOptions("POST", { title }));
  await refreshConversations();
  await selectConversation(value.id);
  $("#prompt").focus();
}

async function selectConversation(id) {
  const value = await api(`/api/conversations/${id}`);
  state.current = value.conversation;
  state.messages = value.messages || [];
  $("#conversation-title").textContent = state.current.title;
  $("#conversation-meta").textContent = `${state.current.model} / ${state.current.backend} / ${state.current.id.slice(0, 8)}`;
  $("#empty-state").classList.add("hidden");
  renderMessages();
  renderConversationList();
  $("#sidebar").classList.remove("open");
  const approval = (value.interrupts || []).find(interrupt => interrupt.id === "human_approval");
  if (approval) {
    showApproval(approval);
  } else {
    state.pendingApproval = null;
    if ($("#approval-dialog").open) $("#approval-dialog").close();
    setRunning(state.running);
  }
}

async function deleteConversation(conversation) {
  if (!confirm(`Delete “${conversation.title}” and all checkpoints?`)) return;
  await api(`/api/conversations/${conversation.id}`, { method: "DELETE" });
  if (state.current?.id === conversation.id) {
    state.current = null;
    state.messages = [];
    $("#messages").replaceChildren();
    $("#empty-state").classList.remove("hidden");
    $("#conversation-title").textContent = "Choose a conversation";
    $("#conversation-meta").textContent = "NO ACTIVE EXPEDITION";
  }
  await refreshConversations();
}

function textFromMessage(message) {
  return (message.content || []).filter(block => block.type === "text").map(block => block.text || "").join("");
}

function renderRichText(container, text) {
  const fragments = String(text || "").split(/```/);
  fragments.forEach((fragment, index) => {
    if (index % 2) {
      const pre = document.createElement("pre");
      const code = document.createElement("code");
      code.textContent = fragment.replace(/^\w+\n/, "");
      pre.append(code);
      container.append(pre);
      return;
    }
    fragment.split(/\n{2,}/).filter(Boolean).forEach(value => {
      const paragraph = document.createElement("p");
      paragraph.textContent = value;
      container.append(paragraph);
    });
  });
}

function renderMessages(streamText = "") {
  const root = $("#messages");
  root.replaceChildren();
  let tokenCount = 0;
  state.messages.forEach(item => {
    const text = textFromMessage(item);
    tokenCount += Math.ceil((text.length + JSON.stringify(item.tool_calls || []).length) / 4);
    if (item.role === "tool") {
      const wrapper = document.createElement("article");
      wrapper.className = "message tool";
      const detail = document.createElement("details");
      detail.className = "tool-card";
      const summary = document.createElement("summary");
      summary.textContent = `${item.name || "tool"} · ${item.tool_status || "success"}`;
      const pre = document.createElement("pre");
      pre.textContent = text;
      detail.append(summary, pre);
      wrapper.append(detail);
      root.append(wrapper);
      return;
    }
    const article = document.createElement("article");
    article.className = `message ${item.role === "human" ? "user" : item.role}`;
    const role = document.createElement("div");
    role.className = "message-role";
    role.textContent = item.role === "human" ? "YOU" : item.role === "assistant" ? "SHELLEY" : item.role;
    const body = document.createElement("div");
    body.className = "message-body";
    renderRichText(body, text);
    (item.content || []).filter(block => ["image", "file", "audio"].includes(block.type)).forEach(block => {
      const attachment = document.createElement("span");
      attachment.className = "attachment-block";
      if (block.type === "image" && block.data) {
        const image = document.createElement("img");
        image.src = `data:${block.mime_type || "image/png"};base64,${block.data}`;
        image.alt = block.name || "attached image";
        attachment.append(image);
      } else {
        attachment.textContent = `▧ ${block.name || block.type}`;
      }
      body.append(attachment);
    });
    if (item.tool_calls?.length) {
      item.tool_calls.forEach(call => {
        const detail = document.createElement("details");
        detail.className = "tool-card";
        const summary = document.createElement("summary");
        summary.textContent = `Requested ${call.name}`;
        const pre = document.createElement("pre");
        pre.textContent = typeof call.arguments === "string" ? call.arguments : JSON.stringify(call.arguments, null, 2);
        detail.append(summary, pre);
        body.append(detail);
      });
    }
    article.append(role, body);
    root.append(article);
  });
  if (streamText) {
    const article = document.createElement("article");
    article.className = "message assistant";
    const role = document.createElement("div"); role.className = "message-role"; role.textContent = "SHELLEY";
    const body = document.createElement("div"); body.className = "message-body token-cursor";
    renderRichText(body, streamText);
    article.append(role, body); root.append(article);
    tokenCount += Math.ceil(streamText.length / 4);
  }
  $("#context-label").textContent = `${tokenCount.toLocaleString()} tokens`;
  $("#context-fill").style.width = `${Math.min(100, tokenCount / 1280)}%`;
  requestAnimationFrame(() => { root.scrollTop = root.scrollHeight; });
}

async function sendMessage() {
  if (state.running) return;
  if (!state.current) await newConversation();
  const prompt = $("#prompt");
  const text = prompt.value.trim();
  if (!text) return;
  const optimistic = { role: "human", content: [{ type: "text", text }] };
  state.messages.push(optimistic);
  renderMessages();
  prompt.value = "";
  autoSizePrompt();
  const attachments = state.attachments.map(item => item.path);
  state.attachments = [];
  renderAttachments();
  setRunning(true);
  let streamed = "";
  try {
    await consumeSSE(`/api/conversations/${state.current.id}/messages`, { message: text, attachments }, payload => {
      if (payload.type === "agent") {
        const event = payload.data;
        if (event.mode === "token" && event.chunk?.message_delta?.content) {
          streamed += event.chunk.message_delta.content.filter(block => block.type === "text").map(block => block.text || "").join("");
          renderMessages(streamed);
        }
        if (event.mode === "interrupt") showApproval(event.interrupt);
      }
      if (payload.type === "error") throw new Error(payload.data.error);
    });
    await selectConversation(state.current.id);
    await refreshConversations();
  } catch (error) {
    toast(error.message, "error");
    await selectConversation(state.current.id).catch(() => {});
  } finally {
    setRunning(false);
  }
}

async function consumeSSE(url, body, onEvent) {
  const response = await fetch(url, jsonOptions("POST", body));
  if (!response.ok) {
    const value = await response.json().catch(() => ({}));
    throw new Error(value.error || response.statusText);
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let boundary;
    while ((boundary = buffer.indexOf("\n\n")) >= 0) {
      const packet = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      let type = "message", data = "";
      packet.split("\n").forEach(line => {
        if (line.startsWith("event:")) type = line.slice(6).trim();
        if (line.startsWith("data:")) data += line.slice(5).trim();
      });
      if (data) onEvent({ type, data: JSON.parse(data) });
    }
  }
}

function showApproval(interrupt) {
  const requests = Array.isArray(interrupt?.value) ? interrupt.value : [];
  if (!requests.length) return;
  state.pendingApproval = requests;
  setRunning(state.running);
  const root = $("#approval-list");
  root.replaceChildren();
  requests.forEach(request => {
    const item = document.createElement("section"); item.className = "approval-item"; item.dataset.callId = request.call.id;
    const head = document.createElement("header"); head.textContent = `${request.call.name} — ${request.description || "approval requested"}`;
    const pre = document.createElement("pre"); pre.textContent = typeof request.call.arguments === "string" ? request.call.arguments : JSON.stringify(request.call.arguments, null, 2);
    const options = document.createElement("div"); options.className = "approval-options";
    ["approve", "reject"].forEach((decision, index) => {
      const label = document.createElement("label");
      const radio = document.createElement("input"); radio.type = "radio"; radio.name = `decision-${request.call.id}`; radio.value = decision; radio.checked = index === 0;
      label.append(radio, document.createTextNode(` ${decision.toUpperCase()}`)); options.append(label);
    });
    item.append(head, pre, options); root.append(item);
  });
  $("#approval-dialog").showModal();
}

async function resumeApproval() {
  if (!state.current || !state.pendingApproval) return;
  const decisions = {};
  state.pendingApproval.forEach(request => {
    const choice = $(`input[name="decision-${CSS.escape(request.call.id)}"]:checked`);
    decisions[request.call.id] = { decision: choice?.value || "reject", reason: choice?.value === "reject" ? "Rejected in Shelley" : "" };
  });
  $("#approval-dialog").close();
  setRunning(true);
  try {
    await consumeSSE(`/api/conversations/${state.current.id}/resume`, { decisions }, payload => {
      if (payload.type === "agent" && payload.data.mode === "interrupt") showApproval(payload.data.interrupt);
      if (payload.type === "error") throw new Error(payload.data.error);
    });
    await selectConversation(state.current.id);
  } catch (error) { toast(error.message, "error"); }
  finally { state.pendingApproval = null; setRunning(false); }
}

async function cancelRun() {
  if (!state.current || !state.running) return;
  await api(`/api/conversations/${state.current.id}/cancel`, { method: "POST" }).catch(error => toast(error.message, "error"));
}

async function uploadFiles(files) {
  for (const file of files) {
    const form = new FormData(); form.append("file", file);
    try {
      const uploaded = await api("/api/upload", { method: "POST", body: form });
      state.attachments.push(uploaded); renderAttachments();
    } catch (error) { toast(error.message, "error"); }
  }
}

function renderAttachments() {
  const root = $("#attachment-strip"); root.replaceChildren();
  state.attachments.forEach((item, index) => {
    const chip = document.createElement("span"); chip.className = "attachment-chip";
    chip.append(document.createTextNode(`▧ ${item.name}`));
    const remove = document.createElement("button"); remove.textContent = "×"; remove.addEventListener("click", () => { state.attachments.splice(index, 1); renderAttachments(); });
    chip.append(remove); root.append(chip);
  });
}

function openPanel(name) {
  state.activePanel = name;
  $("#app").classList.add("panel-open");
  $$(".tool-tab").forEach(button => button.classList.toggle("active", button.dataset.panel === name));
  $$(".panel-view").forEach(panel => panel.classList.add("hidden"));
  $(`#${name}-panel`).classList.remove("hidden");
  $("#panel-title").textContent = name[0].toUpperCase() + name.slice(1);
  if (name === "files") loadFiles();
  if (name === "diff") loadGit();
  if (name === "terminal") $("#terminal-command").focus();
}

function closePanel() {
  state.activePanel = null; $("#app").classList.remove("panel-open");
  $$(".tool-tab").forEach(button => button.classList.remove("active"));
}

async function loadFiles() {
  const query = $("#file-search").value.trim();
  try {
    const value = await api(`/api/files?path=${encodeURIComponent(state.filePath)}&q=${encodeURIComponent(query)}`);
    const entries = value.entries || value.matches || [];
    const root = $("#file-list"); root.replaceChildren(); root.classList.remove("hidden"); $("#editor-shell").classList.add("hidden");
    $("#file-path").textContent = state.filePath;
    if (state.filePath !== "/" && !query) {
      const parent = document.createElement("button"); parent.className = "file-row"; parent.textContent = "↰  ..";
      parent.addEventListener("click", () => { state.filePath = state.filePath.split("/").slice(0, -1).join("/") || "/"; loadFiles(); }); root.append(parent);
    }
    entries.forEach(entry => {
      const row = document.createElement("button"); row.className = "file-row";
      const icon = document.createElement("span"); icon.textContent = entry.is_dir ? "▸" : "·";
      const name = document.createElement("span"); name.textContent = entry.path.split("/").filter(Boolean).pop() || "/";
      const size = document.createElement("small"); size.textContent = entry.is_dir ? "DIR" : `${entry.size || 0} B`;
      row.append(icon, name, size);
      row.addEventListener("click", () => entry.is_dir ? (state.filePath = entry.path.replace(/\/$/, ""), loadFiles()) : openFile(entry.path));
      root.append(row);
    });
  } catch (error) { toast(error.message, "error"); }
}

async function openFile(filePath) {
  try {
    const value = await api(`/api/file?path=${encodeURIComponent(filePath)}`);
    if (value.file_data?.encoding !== "utf-8") { window.open(`/api/download?path=${encodeURIComponent(filePath)}`, "_blank"); return; }
    state.editorPath = filePath;
    $("#editor-path").textContent = filePath;
    $("#file-editor").value = value.file_data?.content || "";
    $("#file-list").classList.add("hidden"); $("#editor-shell").classList.remove("hidden");
  } catch (error) { toast(error.message, "error"); }
}

async function saveFile() {
  if (!state.editorPath) return;
  try { await api("/api/file", jsonOptions("PUT", { path: state.editorPath, content: $("#file-editor").value })); toast(`Saved ${state.editorPath}`); }
  catch (error) { toast(error.message, "error"); }
}

async function loadGit() {
  try {
    const value = await api("/api/git");
    $("#git-status").textContent = value.status.output || "Working tree clean.";
    const root = $("#git-diff"); root.replaceChildren();
    String(value.diff.output || "No unstaged diff.").split("\n").forEach(line => {
      const span = document.createElement("span");
      span.className = line.startsWith("+") && !line.startsWith("+++") ? "diff-add" : line.startsWith("-") && !line.startsWith("---") ? "diff-remove" : line.startsWith("@@") ? "diff-hunk" : "";
      span.textContent = line + "\n"; root.append(span);
    });
  } catch (error) { toast(error.message, "error"); }
}

async function runTerminal(command) {
  if (!command.trim()) return;
  const output = $("#terminal-output");
  output.append(document.createTextNode(`\n$ ${command}\n`));
  try {
    const result = await api("/api/terminal", jsonOptions("POST", { command, timeout_seconds: 120 }));
    output.append(document.createTextNode(result.output || `[exit ${result.exit_code ?? "?"}]\n`));
  } catch (error) { output.append(document.createTextNode(`error: ${error.message}\n`)); }
  output.scrollTop = output.scrollHeight;
}

function openSettings() { $("#settings-dialog").showModal(); }

async function saveSettings() {
  const backend = $('input[name="backend"]:checked')?.value || "local";
  const value = { model: $("#settings-model").value.trim(), backend, sandbox_name: $("#settings-sandbox").value.trim() };
  try { await api("/api/settings", jsonOptions("PUT", value)); await refreshStatus(); $("#settings-dialog").close(); toast("Settings saved"); }
  catch (error) { $("#settings-state").textContent = error.message; }
}

async function saveAPIKey() {
  const key = $("#settings-api-key").value.trim();
  if (!key) return toast("Enter an API key", "error");
  try { await api("/api/auth/api-key", jsonOptions("PUT", { api_key: key })); $("#settings-api-key").value = ""; await refreshStatus(); toast("API key active"); }
  catch (error) { toast(error.message, "error"); }
}

async function pollOAuthStatus() {
  oauthPollTimer = null;
  try {
    await refreshStatus();
  } catch {
    oauthPollTimer = setTimeout(pollOAuthStatus, 1200);
    return;
  }
  if (state.status.oauth_state === "pending") {
    oauthPollTimer = setTimeout(pollOAuthStatus, 1200);
    return;
  }
  if (state.status.oauth_state === "complete") {
    if ($("#settings-dialog").open) $("#settings-dialog").close();
    toast("Subscription sign-in complete");
    $("#prompt").focus();
  } else if (state.status.oauth_state === "failed") {
    toast(state.status.oauth_error || "Subscription sign-in failed", "error");
  }
}

async function startOAuth() {
  try {
    const value = await api("/api/auth/oauth/start", { method: "POST" });
    window.open(value.authorization_url, "shelley-auth", "popup,width=620,height=760");
    $("#oauth-copy").textContent = "Waiting for browser sign-in…";
    clearTimeout(oauthPollTimer);
    oauthPollTimer = setTimeout(pollOAuthStatus, 0);
  } catch (error) { toast(error.message, "error"); }
}

async function clearAuth() {
  await api("/api/auth", { method: "DELETE" }); await refreshStatus(); toast("Credentials cleared");
}

function exportConversation() {
  if (!state.current) return;
  const lines = [`# ${state.current.title}`, ""];
  state.messages.forEach(item => { lines.push(`## ${item.role}`, "", textFromMessage(item), ""); });
  const link = document.createElement("a");
  link.href = URL.createObjectURL(new Blob([lines.join("\n")], { type: "text/markdown" }));
  link.download = `${state.current.title.replace(/[^a-z0-9]+/gi, "-").toLowerCase() || "conversation"}.md`;
  link.click(); URL.revokeObjectURL(link.href);
}

async function forkConversation() {
  if (!state.current) return;
  try { const value = await api(`/api/conversations/${state.current.id}/fork`, { method: "POST" }); await refreshConversations(); await selectConversation(value.id); toast("Conversation forked"); }
  catch (error) { toast(error.message, "error"); }
}

const commands = [
  ["New conversation", () => newConversation(), "⌘N"],
  ["Open files", () => openPanel("files"), "⌘1"],
  ["Review diff", () => openPanel("diff"), "⌘2"],
  ["Open terminal", () => openPanel("terminal"), "⌘3"],
  ["Fork conversation", forkConversation, ""],
  ["Export conversation", exportConversation, ""],
  ["Settings", openSettings, "⌘,"],
  ["Toggle theme", toggleTheme, ""],
];

function showCommands(query = "") {
  const root = $("#command-list"); root.replaceChildren();
  commands.filter(([name]) => name.toLowerCase().includes(query.toLowerCase())).forEach(([name, action, shortcut]) => {
    const row = document.createElement("button"); row.className = "command-row"; row.type = "button"; row.append(document.createTextNode(name));
    if (shortcut) { const key = document.createElement("kbd"); key.textContent = shortcut; row.append(key); }
    row.addEventListener("click", () => { $("#command-dialog").close(); action(); }); root.append(row);
  });
}

function openCommands() { showCommands(); $("#command-dialog").showModal(); $("#command-input").value = ""; $("#command-input").focus(); }
function toggleTheme() { const theme = document.documentElement.dataset.theme === "dark" ? "light" : "dark"; document.documentElement.dataset.theme = theme; localStorage.setItem("shelley-theme", theme); }
function autoSizePrompt() { const prompt = $("#prompt"); prompt.style.height = "auto"; prompt.style.height = `${Math.min(prompt.scrollHeight, 180)}px`; }

function wireEvents() {
  document.addEventListener("click", event => {
    const action = event.target.closest("[data-action]")?.dataset.action;
    if (!action) return;
    ({
      "new-conversation": () => newConversation(), "open-settings": openSettings, "toggle-theme": toggleTheme,
      "open-sidebar": () => $("#sidebar").classList.add("open"), "close-sidebar": () => $("#sidebar").classList.remove("open"),
      "close-panel": closePanel, "refresh-files": loadFiles, "save-file": saveFile, "save-api-key": saveAPIKey,
      "start-oauth": startOAuth, "clear-auth": clearAuth, "fork": forkConversation, "export": exportConversation,
      "review-approval": () => state.pendingApproval && showApproval({ value: state.pendingApproval }),
    }[action]?.());
  });
  $$(".tool-tab").forEach(button => button.addEventListener("click", () => state.activePanel === button.dataset.panel ? closePanel() : openPanel(button.dataset.panel)));
  $("#conversation-search").addEventListener("input", event => refreshConversations(event.target.value).catch(error => toast(error.message, "error")));
  $("#composer").addEventListener("submit", event => { event.preventDefault(); sendMessage(); });
  $("#prompt").addEventListener("input", autoSizePrompt);
  $("#prompt").addEventListener("keydown", event => { if (event.key === "Enter" && !event.shiftKey) { event.preventDefault(); sendMessage(); } });
  $("#stop-button").addEventListener("click", cancelRun);
  $("#attachment-input").addEventListener("change", event => uploadFiles(event.target.files));
  $$("[data-prompt]").forEach(button => button.addEventListener("click", async () => { if (!state.current) await newConversation(); $("#prompt").value = button.dataset.prompt; autoSizePrompt(); $("#prompt").focus(); }));
  $("#file-search").addEventListener("input", () => { clearTimeout(window.fileSearchTimer); window.fileSearchTimer = setTimeout(loadFiles, 180); });
  $("#file-path").addEventListener("click", () => { state.filePath = "/"; loadFiles(); });
  $("#terminal-form").addEventListener("submit", event => { event.preventDefault(); const input = $("#terminal-command"); const command = input.value; input.value = ""; runTerminal(command); });
  $("#settings-form").addEventListener("submit", event => { event.preventDefault(); if (event.submitter?.value === "default") saveSettings(); });
  $$('[data-auth-tab]').forEach(button => button.addEventListener("click", () => { $$('[data-auth-tab]').forEach(item => item.classList.toggle("active", item === button)); $("#auth-api-panel").classList.toggle("hidden", button.dataset.authTab !== "api"); $("#auth-oauth-panel").classList.toggle("hidden", button.dataset.authTab !== "oauth"); }));
  $("#approval-form").addEventListener("submit", event => { event.preventDefault(); if (event.submitter?.value === "default") resumeApproval(); else $("#approval-dialog").close(); });
  $("#command-input").addEventListener("input", event => showCommands(event.target.value));
  $("#messages").addEventListener("scroll", event => { const element = event.target; $("#scroll-bottom").classList.toggle("visible", element.scrollHeight - element.scrollTop - element.clientHeight > 180); });
  $("#scroll-bottom").addEventListener("click", () => { const messages = $("#messages"); messages.scrollTop = messages.scrollHeight; });
  window.addEventListener("focus", () => {
    if (state.status?.oauth_state !== "pending") return;
    clearTimeout(oauthPollTimer);
    oauthPollTimer = setTimeout(pollOAuthStatus, 0);
  });
  document.addEventListener("keydown", event => {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") { event.preventDefault(); openCommands(); }
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "n") { event.preventDefault(); newConversation(); }
    if ((event.metaKey || event.ctrlKey) && event.key === ",") { event.preventDefault(); openSettings(); }
    if (event.key === "Escape") $("#sidebar").classList.remove("open");
  });
}

async function initialize() {
  document.documentElement.dataset.theme = localStorage.getItem("shelley-theme") || (matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");
  wireEvents();
  try {
    await Promise.all([refreshStatus(), refreshConversations()]);
    if (state.conversations[0]) await selectConversation(state.conversations[0].id);
    if (!state.status.ready) openSettings();
  } catch (error) { toast(error.message, "error"); }
}

initialize();
