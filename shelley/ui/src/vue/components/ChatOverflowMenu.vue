<!--
  ChatOverflowMenu.vue — the top-right "kebab" overflow menu.

  This is the first piece of the UI rebuilt on real PrimeVue *components*
  (the rest of the Vue world so far only consumes the PrimeVue *theme*). It
  replaces a hand-rolled dropdown — a `v-if` panel with a manual document
  `mousedown` outside-click listener and bespoke segmented-toggle rows — with:

    - <Popover>     the dropdown surface (dismiss-on-outside-click + Esc + focus
                    trap come for free, so we delete the manual handlers)
    - <SelectButton> the theme / notifications / markdown segmented toggles
    - <Select>      the language picker

  The e2e DOM/ARIA contract is preserved so the shared Playwright specs keep
  passing in BOTH worlds:
    - root wrapper:  .chat-overflow-menu-wrapper
    - trigger:       button.btn-icon  (aria-label = t('moreOptions'))
    - action items:  button.overflow-menu-item  (matched by visible text)
  See e2e/agents-md-vim.spec.ts and e2e/diff-viewer-find.spec.ts.

  State the menu reads/writes lives in shared composables/services
  (markdownMode, theme, notifications), so this component owns it directly
  instead of taking a dozen props. Conversation-scoped actions (diffs, git
  graph, archive, export, …) are surfaced as events for ChatInterface to wire
  to its existing handlers.
-->
<template>
  <div class="chat-overflow-menu-wrapper">
    <Button
      class="btn-icon"
      text
      severity="secondary"
      :aria-label="t('moreOptions')"
      aria-haspopup="true"
      :aria-expanded="open"
      @click="toggle"
    >
      <svg fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path
          stroke-linecap="round"
          stroke-linejoin="round"
          :stroke-width="2"
          d="M12 5v.01M12 12v.01M12 19v.01M12 6a1 1 0 110-2 1 1 0 010 2zm0 7a1 1 0 110-2 1 1 0 010 2zm0 7a1 1 0 110-2 1 1 0 010 2z"
        />
      </svg>
      <span v-if="hasUpdate" class="version-update-dot" />
    </Button>

    <Popover
      ref="popoverRef"
      :pt="{ root: { class: 'chat-overflow-popover' }, content: { class: 'overflow-menu-panel' } }"
      @show="open = true"
      @hide="open = false"
    >
      <!-- Conversation / workspace actions -->
      <button v-if="hasCwd" class="overflow-menu-item" @click="onDiffs">
        <!-- Diffs: two rows of +/- changes -->
        <svg
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
          viewBox="0 0 24 24"
          class="chat-menu-icon"
          aria-hidden="true"
        >
          <path d="M4 7h4M6 5v4" />
          <path d="M14 7h6" />
          <path d="M4 17h6" />
          <path d="M14 17h6M17 15v4" />
        </svg>
        {{ t("diffs") }}
        <span class="overflow-menu-shortcut"
          ><kbd>{{ menuShortcutLabel("diffs") }}</kbd></span
        >
      </button>
      <button v-if="hasCwd" class="overflow-menu-item" @click="onGitGraph">
        <!-- Git graph: commits A (top) and B (top-right) branching from C (bottom) -->
        <svg
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
          viewBox="0 0 24 24"
          class="chat-menu-icon"
          aria-hidden="true"
        >
          <path d="M6 16.6V7.4" />
          <path d="M6 16.6C8 11 12 6 14.6 5" />
          <circle cx="6" cy="5" r="2.4" />
          <circle cx="17" cy="5" r="2.4" />
          <circle cx="6" cy="19" r="2.4" />
        </svg>
        {{ t("gitGraph") }}
        <span class="overflow-menu-shortcut"
          ><kbd>{{ menuShortcutLabel("gitGraph") }}</kbd></span
        >
      </button>
      <button class="overflow-menu-item" @click="onTerminal">
        <i class="pi pi-desktop chat-menu-icon" aria-hidden="true" />
        {{ t("terminal") }}
        <span class="overflow-menu-shortcut"
          ><kbd>{{ menuShortcutLabel("terminal") }}</kbd></span
        >
      </button>

      <!-- Custom server-provided links (icon is a raw SVG path) -->
      <button
        v-for="(link, index) in links"
        :key="index"
        class="overflow-menu-item"
        @click="onExternalLink(link.url)"
      >
        <svg fill="none" stroke="currentColor" viewBox="0 0 24 24" class="chat-menu-icon">
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            :stroke-width="2"
            :d="
              link.icon_svg ||
              'M10 6H6a2 2 0 00-2 2v10a2 2 0 002 2h10a2 2 0 002-2v-4M14 4h6m0 0v6m0-6L10 14'
            "
          />
        </svg>
        {{ link.title }}
      </button>

      <template v-if="canArchive">
        <div class="overflow-menu-divider" />
        <button class="overflow-menu-item" @click="onArchive">
          <i class="pi pi-inbox chat-menu-icon" aria-hidden="true" />
          {{ t("archiveConversation") }}
          <span class="overflow-menu-shortcut"
            ><kbd>{{ menuShortcutLabel("archive") }}</kbd></span
          >
        </button>
      </template>

      <template v-if="canExport">
        <div class="overflow-menu-divider" />
        <button class="overflow-menu-item" @click="onExport">
          <i class="pi pi-download chat-menu-icon" aria-hidden="true" />
          {{ t("exportConversation") }}
          <span class="overflow-menu-shortcut"
            ><kbd>{{ menuShortcutLabel("export") }}</kbd></span
          >
        </button>
      </template>

      <div class="overflow-menu-divider" />
      <button class="overflow-menu-item" @click="onEditAgentsMd">
        <i class="pi pi-pencil chat-menu-icon" aria-hidden="true" />
        {{ t("editUserAgentsMd") }}
        <span class="overflow-menu-shortcut"
          ><kbd>{{ menuShortcutLabel("editAgentsMd") }}</kbd></span
        >
      </button>
      <button class="overflow-menu-item" @click="onEditFile">
        <i class="pi pi-file-edit chat-menu-icon" aria-hidden="true" />
        {{ t("editFile") }}
        <span
          v-tooltip.bottom="editFileShortcutTooltip"
          class="overflow-menu-shortcut"
          :class="{ 'overflow-menu-shortcut-inert': isFirefox }"
          ><kbd>{{ menuShortcutLabel("editFile") }}</kbd></span
        >
      </button>

      <div class="overflow-menu-divider" />
      <button class="overflow-menu-item" @click="onCheckVersion">
        <i class="pi pi-refresh chat-menu-icon" aria-hidden="true" />
        {{ t("checkForNewVersion") }}
        <span v-if="hasUpdate" class="version-menu-dot" />
        <span class="overflow-menu-shortcut"
          ><kbd>{{ menuShortcutLabel("checkVersion") }}</kbd></span
        >
      </button>

      <!-- Theme: System / Light / Dark -->
      <div class="overflow-menu-divider" />
      <div class="overflow-menu-control">
        <SelectButton
          v-model="theme"
          :options="themeOptions"
          option-value="value"
          data-key="value"
          :allow-empty="false"
          :aria-label="t('system') + ' / ' + t('light') + ' / ' + t('dark')"
          @update:model-value="onThemeChange"
        >
          <template #option="{ option }">
            <i :class="['pi', option.icon]" :title="option.label" aria-hidden="true" />
            <span class="sr-only-label">{{ option.label }}</span>
          </template>
        </SelectButton>
      </div>

      <!-- Browser notifications: on / off -->
      <template v-if="notificationSupported">
        <div class="overflow-menu-divider" />
        <div class="overflow-menu-control">
          <SelectButton
            v-model="notifEnabled"
            :options="notifOptions"
            option-value="value"
            option-disabled="disabled"
            data-key="value"
            :allow-empty="false"
            :aria-label="t('enableNotifications') + ' / ' + t('disableNotifications')"
            @update:model-value="onNotifChange"
          >
            <template #option="{ option }">
              <i :class="['pi', option.icon]" :title="option.label" aria-hidden="true" />
              <span class="sr-only-label">{{ option.label }}</span>
            </template>
          </SelectButton>
        </div>
      </template>

      <!-- Language -->
      <div class="overflow-menu-divider" />
      <div class="overflow-menu-control">
        <div class="md-toggle-label">{{ t("language") }}</div>
        <Select
          v-model="lang"
          :options="languageOptions"
          option-label="label"
          option-value="locale"
          :aria-label="t('switchLanguage')"
          class="overflow-language-select"
          append-to="self"
          @update:model-value="onLangChange"
        >
          <template #value="{ value }">
            <span class="language-dropdown-flag">{{ languageFor(value).flag }}</span>
            <span>{{ languageFor(value).label }}</span>
          </template>
          <template #option="{ option }">
            <span class="language-dropdown-flag">{{ option.flag }}</span>
            <span>{{ option.label }}</span>
          </template>
        </Select>
      </div>
    </Popover>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from "vue";
import Popover from "primevue/popover";
import Button from "primevue/button";
import SelectButton from "primevue/selectbutton";
import Select from "primevue/select";
import type { Link } from "../../types";
import type { Locale } from "../../i18n/types";
import { useI18n } from "../composables/i18n";
import { menuShortcutLabel, isFirefox } from "../../utils/menuShortcuts";
import { type ThemeMode, getStoredTheme, setStoredTheme, applyTheme } from "../../services/theme";
import {
  isChannelEnabled,
  setChannelEnabled,
  getBrowserNotificationState,
  requestBrowserNotificationPermission,
} from "../../services/notifications";

defineProps<{
  hasCwd: boolean;
  links: Link[];
  canArchive: boolean;
  canExport: boolean;
  hasUpdate: boolean;
}>();

const emit = defineEmits<{
  (e: "open-diffs"): void;
  (e: "open-git-graph"): void;
  (e: "open-terminal"): void;
  (e: "open-external-link", url: string): void;
  (e: "archive"): void;
  (e: "export"): void;
  (e: "edit-agents-md"): void;
  (e: "edit-file"): void;
  (e: "check-version"): void;
}>();

const { t, locale, setLocale } = useI18n();

// Edit File uses Cmd/Ctrl+Shift+P (VS Code parity). Firefox reserves that combo
// for "New Private Window" and never delivers it to the page, so the shortcut
// is inert there; explain that on hover rather than silently misleading users.
const editFileShortcutTooltip = computed(() =>
  isFirefox ? t("editFileShortcutFirefox") : t("editFileShortcut"),
);

const popoverRef = ref<InstanceType<typeof Popover> | null>(null);
const open = ref(false);

function toggle(event: MouseEvent) {
  popoverRef.value?.toggle(event);
}
function hide() {
  popoverRef.value?.hide();
}

// Each action emits its event, then closes the Popover. Kept as explicit
// one-liners (rather than a union-typed helper) so defineEmits' per-event
// overloads type-check cleanly.
const onDiffs = () => (emit("open-diffs"), hide());
const onGitGraph = () => (emit("open-git-graph"), hide());
const onTerminal = () => (emit("open-terminal"), hide());
const onArchive = () => (emit("archive"), hide());
const onExport = () => (emit("export"), hide());
const onEditAgentsMd = () => (emit("edit-agents-md"), hide());
const onEditFile = () => (emit("edit-file"), hide());
const onCheckVersion = () => (emit("check-version"), hide());
function onExternalLink(url: string) {
  emit("open-external-link", url);
  hide();
}

const notificationSupported = typeof Notification !== "undefined";

// ---- Theme toggle (System / Light / Dark) ----
// Option labels are computed so they re-translate when the locale changes (the
// language Select lives in this same popover, so a switch is visible live).
const theme = ref<ThemeMode>(getStoredTheme());
const themeOptions = computed(() => [
  { value: "system" as ThemeMode, icon: "pi-desktop", label: t("system") },
  { value: "light" as ThemeMode, icon: "pi-sun", label: t("light") },
  { value: "dark" as ThemeMode, icon: "pi-moon", label: t("dark") },
]);
function onThemeChange(mode: ThemeMode) {
  theme.value = mode;
  setStoredTheme(mode);
  applyTheme(mode);
}

// ---- Browser notifications (on / off) ----
const notifEnabled = ref<boolean>(isChannelEnabled("browser"));
const notifBlocked = getBrowserNotificationState() === "denied";
const notifOptions = computed(() => [
  {
    value: true,
    icon: "pi-bell",
    label: notifBlocked ? t("blockedByBrowser") : t("enableNotifications"),
    disabled: notifBlocked,
  },
  { value: false, icon: "pi-bell-slash", label: t("disableNotifications"), disabled: false },
]);
async function onNotifChange(next: boolean) {
  if (next) {
    // v-model has already flipped notifEnabled to true; confirm via the
    // permission prompt and revert if the user denies.
    const granted = await requestBrowserNotificationPermission();
    notifEnabled.value = granted;
  } else {
    setChannelEnabled("browser", false);
    notifEnabled.value = false;
  }
}

// ---- Language picker ----
interface LanguageOption {
  locale: Locale;
  flag: string;
  label: string;
}
const languageOptions: LanguageOption[] = [
  { locale: "en", flag: "\uD83C\uDDFA\uD83C\uDDF8", label: "English" },
  { locale: "ja", flag: "\uD83C\uDDEF\uD83C\uDDF5", label: "\u65E5\u672C\u8A9E" },
  { locale: "fr", flag: "\uD83C\uDDEB\uD83C\uDDF7", label: "Fran\u00E7ais" },
  {
    locale: "ru",
    flag: "\uD83C\uDDF7\uD83C\uDDFA",
    label: "\u0420\u0443\u0441\u0441\u043A\u0438\u0439",
  },
  { locale: "es", flag: "\uD83C\uDDEA\uD83C\uDDF8", label: "Espa\u00F1ol" },
  { locale: "zh-CN", flag: "\uD83C\uDDE8\uD83C\uDDF3", label: "\u7B80\u4F53\u4E2D\u6587" },
  { locale: "zh-TW", flag: "\uD83C\uDDF9\uD83C\uDDFC", label: "\u7E41\u9AD4\u4E2D\u6587" },
  { locale: "vi", flag: "\uD83C\uDDFB\uD83C\uDDF3", label: "Ti\u1EBFng Vi\u1EC7t" },
  { locale: "upgoer5", flag: "\uD83D\uDE80", label: "Up-Goer Five" },
];
const lang = ref<Locale>(locale.value);
function languageFor(l: Locale): LanguageOption {
  return languageOptions.find((o) => o.locale === l) || languageOptions[0];
}
function onLangChange(l: Locale) {
  lang.value = l;
  setLocale(l);
}
</script>
