<template>
  <Modal
    :is-open="isOpen"
    title="Set up browser workspace"
    class-name="browser-key-modal"
    :closable="false"
  >
    <section class="browser-directory" aria-labelledby="browser-directory-title">
      <div>
        <span class="browser-setup-kicker">Workspace</span>
        <h3 id="browser-directory-title">Open a project folder</h3>
        <p>
          Work directly in a folder you choose. .git, node_modules, and oversized files stay
          untouched. The active model only receives files the agent reads.
        </p>
      </div>
      <Button
        type="button"
        icon="pi pi-folder-open"
        :label="
          directoryReconnect ? 'Reconnect folder' : directoryInfo ? 'Change folder' : 'Open folder'
        "
        :loading="operation === 'directory'"
        :disabled="operation !== null || !directorySupported"
        @click="openDirectory"
      />
      <div v-if="directoryInfo" class="browser-directory-status" aria-live="polite">
        <span class="browser-directory-dot" aria-hidden="true" />
        <span>
          {{ directoryInfo.name }} · {{ directoryInfo.fileCount.toLocaleString() }} files
          <template v-if="directoryInfo.skippedCount">
            · {{ directoryInfo.skippedCount.toLocaleString() }} excluded
          </template>
        </span>
      </div>
      <p v-else-if="operation === 'directory'" class="browser-directory-status" aria-live="polite">
        {{ directoryProgressText }}
      </p>
      <p v-else-if="!directorySupported" class="browser-directory-status">
        Folder access requires Chrome or Edge on desktop.
      </p>
      <button
        v-if="directoryReconnect"
        type="button"
        class="browser-directory-skip"
        :disabled="operation !== null"
        @click="useBrowserWorkspace"
      >
        Use browser workspace instead
      </button>
    </section>

    <div class="browser-key-divider"><span>Choose a model</span></div>

    <section class="browser-local-model" aria-labelledby="browser-local-model-title">
      <div>
        <h3 id="browser-local-model-title">Run locally with WebGPU</h3>
        <p>
          Qwen3.5 0.8B runs privately on your GPU. The compact 4-bit model downloads once, then
          stays cached by your browser.
        </p>
      </div>
      <Button
        type="button"
        label="Use local model"
        :loading="operation === 'local'"
        :disabled="operation !== null"
        @click="useLocalModel"
      />
      <div v-if="operation === 'local'" class="browser-local-progress" aria-live="polite">
        <progress :value="localProgress" max="1" />
        <span>{{ localProgressText || `Loading… ${Math.round(localProgress * 100)}%` }}</span>
      </div>
    </section>

    <div class="browser-key-divider"><span>or connect OpenAI</span></div>

    <form class="browser-key-form" @submit.prevent="save">
      <label class="browser-key-label" for="browser-openai-key">OpenAI API key</label>
      <input
        id="browser-openai-key"
        ref="keyInput"
        v-model="apiKey"
        data-testid="browser-openai-key-input"
        class="browser-key-input"
        type="password"
        autocomplete="off"
        spellcheck="false"
        placeholder="sk-…"
        :disabled="operation !== null"
      />
      <p class="browser-key-note">
        Saved in this browser so you stay connected after reloading the page.
      </p>
      <p v-if="error" class="browser-key-error" role="alert">{{ error }}</p>

      <div class="browser-key-actions">
        <Button
          type="submit"
          :label="operation === 'openai' ? 'Connecting…' : 'Continue'"
          :loading="operation === 'openai'"
          :disabled="operation !== null || !apiKey.trim()"
        />
      </div>
    </form>
  </Modal>
</template>

<script setup lang="ts">
import { nextTick, ref, watch } from "vue";
import Button from "primevue/button";
import Modal from "./Modal.vue";
import {
  browserConnectedDirectory,
  browserLocalDirectoryReconnectRequired,
  browserLocalDirectorySupported,
  browserModelConfigured,
  configureBrowserOpenAIKey,
  configureBrowserWebGPUModel,
  connectBrowserLocalDirectory,
  useBrowserWorkspaceInstead,
} from "../../services/wasmRuntime";
import type { BrowserDirectoryInfo } from "../../services/browserDirectory";

const props = defineProps<{ isOpen: boolean }>();
const emit = defineEmits<{
  (event: "configured", model: string): void;
  (event: "directory-connected"): void;
}>();

const apiKey = ref("");
const error = ref("");
const operation = ref<"directory" | "local" | "openai" | null>(null);
const localProgress = ref(0);
const localProgressText = ref("");
const directoryProgressText = ref("Reading project files…");
const directorySupported = browserLocalDirectorySupported();
const directoryReconnect = ref(browserLocalDirectoryReconnectRequired());
const directoryInfo = ref<BrowserDirectoryInfo | null>(browserConnectedDirectory());
const keyInput = ref<HTMLInputElement | null>(null);

watch(
  () => props.isOpen,
  async (open) => {
    if (!open) return;
    await nextTick();
    keyInput.value?.focus();
  },
  { immediate: true },
);

async function save() {
  if (operation.value) return;
  operation.value = "openai";
  error.value = "";
  try {
    const model = await configureBrowserOpenAIKey(apiKey.value);
    apiKey.value = "";
    emit("configured", model);
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : String(reason);
  } finally {
    operation.value = null;
  }
}

async function useLocalModel() {
  if (operation.value) return;
  operation.value = "local";
  localProgress.value = 0;
  localProgressText.value = "Preparing WebGPU…";
  error.value = "";
  try {
    const model = await configureBrowserWebGPUModel((progress, text) => {
      localProgress.value = progress;
      localProgressText.value = text;
    });
    emit("configured", model);
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : String(reason);
  } finally {
    operation.value = null;
  }
}

async function openDirectory() {
  if (operation.value) return;
  operation.value = "directory";
  directoryProgressText.value = "Waiting for folder…";
  error.value = "";
  try {
    directoryInfo.value = await connectBrowserLocalDirectory((text) => {
      directoryProgressText.value = text;
    });
    directoryReconnect.value = false;
    emit("directory-connected");
  } catch (reason) {
    if (!(reason instanceof DOMException && reason.name === "AbortError")) {
      error.value = reason instanceof Error ? reason.message : String(reason);
    }
  } finally {
    operation.value = null;
  }
}

async function useBrowserWorkspace() {
  if (operation.value) return;
  operation.value = "directory";
  error.value = "";
  try {
    await useBrowserWorkspaceInstead();
    directoryReconnect.value = false;
    directoryInfo.value = null;
    if (browserModelConfigured()) emit("directory-connected");
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : String(reason);
  } finally {
    operation.value = null;
  }
}
</script>

<style scoped>
.browser-key-form {
  box-sizing: border-box;
  width: 100%;
  padding: 0.35rem 0 0.25rem;
}

.browser-directory {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: center;
  gap: 0.7rem 1rem;
  padding: 0.25rem 0 0.1rem;
}

.browser-setup-kicker {
  display: block;
  margin-bottom: 0.35rem;
  color: var(--accent, #e6aa24);
  font-size: 0.64rem;
  font-weight: 700;
  letter-spacing: 0.11em;
  text-transform: uppercase;
}

.browser-directory h3 {
  margin: 0 0 0.35rem;
  color: var(--text-primary);
  font-size: 0.92rem;
}

.browser-directory p {
  margin: 0;
  color: var(--text-muted, var(--text-secondary));
  font-size: 0.76rem;
  line-height: 1.45;
}

.browser-directory-status {
  display: flex;
  grid-column: 1 / -1;
  align-items: center;
  gap: 0.45rem;
  color: var(--text-muted, var(--text-secondary));
  font-size: 0.72rem;
}

.browser-directory-dot {
  width: 0.45rem;
  height: 0.45rem;
  border-radius: 50%;
  background: #51b781;
  box-shadow: 0 0 0 3px color-mix(in srgb, #51b781 16%, transparent);
}

.browser-directory-skip {
  grid-column: 1 / -1;
  justify-self: start;
  border: 0;
  padding: 0;
  background: transparent;
  color: var(--text-secondary);
  cursor: pointer;
  font: inherit;
  font-size: 0.72rem;
  text-decoration: underline;
  text-underline-offset: 0.18rem;
}

.browser-directory-skip:disabled {
  cursor: default;
  opacity: 0.55;
}

.browser-local-model {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: center;
  gap: 0.85rem 1rem;
  padding: 0.35rem 0 0.25rem;
}

.browser-local-model h3 {
  margin: 0 0 0.35rem;
  color: var(--text-primary);
  font-size: 0.92rem;
}

.browser-local-model p {
  margin: 0;
  color: var(--text-muted, var(--text-secondary));
  font-size: 0.76rem;
  line-height: 1.45;
}

.browser-local-progress {
  display: grid;
  grid-column: 1 / -1;
  gap: 0.35rem;
  color: var(--text-muted, var(--text-secondary));
  font-size: 0.72rem;
}

.browser-local-progress progress {
  width: 100%;
  accent-color: var(--accent, #e6aa24);
}

.browser-key-divider {
  display: flex;
  align-items: center;
  gap: 0.75rem;
  margin: 1.25rem 0 0.8rem;
  color: var(--text-muted, var(--text-secondary));
  font-size: 0.72rem;
  text-transform: uppercase;
  letter-spacing: 0.06em;
}

.browser-key-divider::before,
.browser-key-divider::after {
  height: 1px;
  flex: 1;
  background: var(--border-color);
  content: "";
}

.browser-key-label {
  display: block;
  margin-bottom: 0.5rem;
  color: var(--text-primary);
  font-size: 0.78rem;
  font-weight: 650;
  letter-spacing: 0.04em;
}

.browser-key-input {
  box-sizing: border-box;
  width: 100%;
  height: 3rem;
  border: 1px solid color-mix(in srgb, var(--border-color) 72%, var(--accent, #e6aa24));
  border-radius: 0;
  outline: none;
  background: color-mix(in srgb, var(--bg-primary) 88%, black);
  color: var(--text-primary);
  font-family: var(--font-mono, monospace);
  font-size: 0.95rem;
  letter-spacing: 0.035em;
  padding: 0 0.9rem;
  transition:
    border-color 120ms ease,
    box-shadow 120ms ease;
}

.browser-key-input:focus {
  border-color: var(--accent, #e6aa24);
  box-shadow: 0 0 0 1px var(--accent, #e6aa24);
}

.browser-key-note {
  margin: 0.55rem 0 0;
  color: var(--text-muted, var(--text-secondary));
  font-size: 0.74rem;
}

.browser-key-error {
  margin: 0.85rem 0 0;
  color: var(--danger, #f06a62);
  font-size: 0.82rem;
}

.browser-key-actions {
  display: flex;
  justify-content: flex-end;
  gap: 0.65rem;
  margin-top: 1.75rem;
}
</style>
