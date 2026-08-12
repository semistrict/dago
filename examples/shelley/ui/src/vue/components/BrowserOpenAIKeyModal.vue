<template>
  <Modal :is-open="isOpen" title="Connect OpenAI" class-name="browser-key-modal" :closable="false">
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
        :disabled="saving"
      />
      <p class="browser-key-note">
        Saved in this browser so you stay connected after reloading the page.
      </p>
      <p v-if="error" class="browser-key-error" role="alert">{{ error }}</p>

      <div class="browser-key-actions">
        <Button
          type="submit"
          :label="saving ? 'Connecting…' : 'Continue'"
          :loading="saving"
          :disabled="saving || !apiKey.trim()"
        />
      </div>
    </form>
  </Modal>
</template>

<script setup lang="ts">
import { nextTick, ref, watch } from "vue";
import Button from "primevue/button";
import Modal from "./Modal.vue";
import { configureBrowserOpenAIKey } from "../../services/wasmRuntime";

const props = defineProps<{ isOpen: boolean }>();
const emit = defineEmits<{ (event: "configured", model: string): void }>();

const apiKey = ref("");
const error = ref("");
const saving = ref(false);
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
  if (saving.value) return;
  saving.value = true;
  error.value = "";
  try {
    const model = await configureBrowserOpenAIKey(apiKey.value);
    apiKey.value = "";
    emit("configured", model);
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : String(reason);
  } finally {
    saving.value = false;
  }
}
</script>

<style scoped>
.browser-key-form {
  box-sizing: border-box;
  width: 100%;
  padding: 0.35rem 0 0.25rem;
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
