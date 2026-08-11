// Vue 3 + PrimeVue entry point. Emits dist/main.js + dist/main.css, mounts into
// #root, and handles routing (/export/<id> vs the chat app) plus app-wide
// side-effects (theme, notifications).
import { createApp } from "vue";
import PrimeVue from "primevue/config";
import Tooltip from "primevue/tooltip";
import FocusTrap from "primevue/focustrap";
import { ShelleyPreset } from "./theme/shelley-preset";

import "primeicons/primeicons.css";
import "@xterm/xterm/css/xterm.css";

import { initializeTheme } from "../services/theme";
import { initializeNotifications } from "../services/notifications";
import { i18nPlugin } from "./composables/i18n";
import { exportConversationIdFromPath } from "./export";
import { browserWasmBuild } from "../basePath";
import App from "./App.vue";
import ExportPage from "./components/ExportPage.vue";

async function initializeRuntime(): Promise<void> {
  const runtimeKey = "shelley_runtime";
  const queryRuntime = new URLSearchParams(window.location.search).get("runtime");
  if (queryRuntime === "wasm" || browserWasmBuild) sessionStorage.setItem(runtimeKey, "wasm");
  const selected = queryRuntime || sessionStorage.getItem(runtimeKey);
  if (selected !== "wasm") return;
  const { installWasmRuntime } = await import("../services/wasmRuntime");
  await installWasmRuntime();
}

// Apply theme before render to avoid a flash of the wrong color scheme.
initializeTheme();

const rootElement = document.getElementById("root");
if (!rootElement) throw new Error("Root container not found");
const rootContainer: HTMLElement = rootElement;

const primeVueOptions = {
  theme: {
    preset: ShelleyPreset,
    options: {
      // Match the legacy app's dark-mode contract: <html class="dark">.
      darkModeSelector: ".dark",
      // Keep PrimeVue's utility/reset layers from overriding our styles.css.
      cssLayer: { name: "primevue", order: "primevue" },
    },
  },
};

async function mount(): Promise<void> {
  await initializeRuntime();
  const exportId = exportConversationIdFromPath();
  if (exportId) {
    // Standalone, read-mostly export view. No notifications/app side-effects.
    const app = createApp(ExportPage, { conversationId: exportId });
    app.use(PrimeVue, primeVueOptions);
    app.use(i18nPlugin);
    app.directive("tooltip", Tooltip);
    app.mount(rootContainer);
  } else {
    initializeNotifications();
    const app = createApp(App);
    app.use(PrimeVue, primeVueOptions);
    app.use(i18nPlugin);
    app.directive("tooltip", Tooltip);
    // Used by the hand-rolled full-screen overlays (PrimeVue's Dialog, behind
    // Modal.vue, engages the trap itself).
    app.directive("focustrap", FocusTrap);
    app.mount(rootContainer);
  }
}

void mount().catch((error) => {
  console.error("Failed to initialize Shelley:", error);
  rootContainer.textContent = `Failed to initialize Shelley: ${error instanceof Error ? error.message : String(error)}`;
});
