<template>
  <Modal :is-open="isOpen" title="Version" class-name="version-modal" @close="emit('close')">
    <div v-if="isLoading" class="version-loading">Loading...</div>
    <template v-else-if="versionInfo">
      <div class="version-info-row">
        <span class="version-label">Version:</span>
        <span class="version-value">{{ displayVersion }}</span>
      </div>
      <div v-if="versionInfo.current_commit" class="version-info-row">
        <span class="version-label">Commit:</span>
        <span class="version-value">{{ versionInfo.current_commit }}</span>
      </div>
    </template>
    <div v-else class="version-loading">Version information is unavailable.</div>
  </Modal>
</template>

<script setup lang="ts">
import { computed } from "vue";
import type { VersionInfo } from "../../types";
import Modal from "./Modal.vue";

const props = defineProps<{
  isOpen: boolean;
  versionInfo: VersionInfo | null;
  isLoading: boolean;
}>();
const emit = defineEmits<{ (event: "close"): void }>();

const displayVersion = computed(
  () => props.versionInfo?.current_tag || props.versionInfo?.current_version || "development build",
);
</script>
