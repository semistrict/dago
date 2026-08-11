import { onMounted, ref } from "vue";
import { api } from "../../services/api";
import type { VersionInfo } from "../../types";

export function useVersionChecker() {
  const versionInfo = ref<VersionInfo | null>(null);
  const showModal = ref(false);
  const isLoading = ref(false);

  async function loadVersion() {
    isLoading.value = true;
    try {
      const info = await api.checkVersion(false);
      versionInfo.value = info;
    } catch (err) {
      console.error("Failed to check version:", err);
    } finally {
      isLoading.value = false;
    }
  }

  onMounted(loadVersion);

  function openModal() {
    showModal.value = true;
    loadVersion();
  }

  function closeModal() {
    showModal.value = false;
  }

  return {
    hasUpdate: ref(false),
    versionInfo,
    showModal,
    isLoading,
    openModal,
    closeModal,
  };
}
