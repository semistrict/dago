<!-- Vue port of components/PatchTool.tsx. Drives the framework-agnostic
     @pierre/diffs FileDiff renderer (via the useFileDiffInstance composable)
     against the shared syntax-highlighting worker pool, matching React's
     <PatchDiff>/<MultiFileDiff>.

     Approach: getSingularPatch (unified diff strings) / parseDiffFromFile
     (old/new file snapshots) parse the diff into FileDiffMetadata; the
     composable creates a <diffs-container> custom element and hydrates a
     FileDiff instance with the worker pool, so shiki/TextMate tokenization runs
     off the main thread. The previous SSR path (preloadPatchDiff /
     preloadDiffHTML) ran synchronous WASM highlighting on the main thread,
     which froze the UI for seconds in conversations with many diffs.

     Error handling: parsing failures set diffError and fall back to a raw
     <pre>, replacing the React class-based error boundary.

     Preserves: .patch-tool, .patch-tool-details, .patch-tool-header,
     .patch-tool-toggle, .patch-tool-emoji, data-testid tool-call-completed,
     and all other classes from the React original. -->
<template>
  <div class="patch-tool" :data-testid="isComplete ? 'tool-call-completed' : 'tool-call-running'">
    <div class="patch-tool-header" @click="isExpanded = !isExpanded">
      <div class="patch-tool-summary">
        <span class="patch-tool-emoji" :class="{ running: isRunning }">🖋️</span>
        <span class="patch-tool-filename" :title="filename">{{ filename }}</span>
        <span v-if="isComplete && hasError" class="patch-tool-error">✗</span>
        <span v-if="isComplete && !hasError" class="patch-tool-success">✓</span>
      </div>
      <div class="patch-tool-header-controls">
        <button
          v-if="showDiffToggle"
          v-tooltip.top="sideBySide ? 'Switch to inline diff' : 'Switch to side-by-side diff'"
          class="patch-tool-diff-mode-toggle"
          :aria-label="sideBySide ? 'Switch to inline diff' : 'Switch to side-by-side diff'"
          @click.stop="toggleSideBySide"
        >
          <svg
            width="14"
            height="14"
            viewBox="0 0 14 14"
            fill="none"
            xmlns="http://www.w3.org/2000/svg"
          >
            <template v-if="sideBySide">
              <!-- Side-by-side icon (two columns) -->
              <rect
                x="1"
                y="2"
                width="5"
                height="10"
                rx="1"
                stroke="currentColor"
                stroke-width="1.5"
                fill="none"
              />
              <rect
                x="8"
                y="2"
                width="5"
                height="10"
                rx="1"
                stroke="currentColor"
                stroke-width="1.5"
                fill="none"
              />
            </template>
            <template v-else>
              <!-- Inline icon (single column with horizontal lines) -->
              <rect
                x="2"
                y="2"
                width="10"
                height="10"
                rx="1"
                stroke="currentColor"
                stroke-width="1.5"
                fill="none"
              />
              <line x1="4" y1="5" x2="10" y2="5" stroke="currentColor" stroke-width="1.5" />
              <line x1="4" y1="9" x2="10" y2="9" stroke="currentColor" stroke-width="1.5" />
            </template>
          </svg>
        </button>
        <button
          class="patch-tool-toggle"
          :aria-label="isExpanded ? 'Collapse' : 'Expand'"
          :aria-expanded="isExpanded"
        >
          <svg
            width="12"
            height="12"
            viewBox="0 0 12 12"
            fill="none"
            xmlns="http://www.w3.org/2000/svg"
            class="tool-chevron"
            :class="{ 'tool-chevron-expanded': isExpanded }"
          >
            <path
              d="M4.5 3L7.5 6L4.5 9"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
            />
          </svg>
        </button>
      </div>
    </div>

    <div v-if="isExpanded" class="patch-tool-details">
      <div v-if="isComplete && !hasError && hasDiff" class="patch-tool-section">
        <div class="patch-tool-diffs-container">
          <!-- The FileDiff renderer (driven by useFileDiffInstance) creates its
               own <diffs-container> custom element here and renders into its
               shadow root, so the diff's scoped <style> blocks never leak into
               the page. Highlighting runs off the main thread via the shared
               @pierre/diffs worker pool, matching React's <PatchDiff>.

               Hydration is deferred until the tool is near the viewport
               (useNearViewport): a huge conversation can hold hundreds of
               diffs, and hydrating them all adds ~200k shadow-DOM elements
               that make every window resize re-lay-out for seconds. -->
          <div
            ref="diffHostEl"
            class="patch-tool-diff-host"
            :style="rendered ? undefined : { minHeight: placeholderHeight }"
          ></div>
          <pre v-if="diffError && rawDiff" class="patch-tool-raw-diff">{{ rawDiff }}</pre>
        </div>
      </div>

      <div v-if="isComplete && hasError" class="patch-tool-section">
        <pre class="patch-tool-error-message">{{ errorMessage || "Patch failed" }}</pre>
      </div>

      <div v-if="isRunning" class="patch-tool-section">
        <div class="patch-tool-label">Applying patch...</div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, onMounted, onUnmounted } from "vue";
import type { LLMContent } from "../../../types";
import type {
  FileContents,
  SupportedLanguages,
  ThemeTypes,
  ThemesType,
  FileDiffMetadata,
  FileDiffOptions,
} from "@pierre/diffs";
import { getSingularPatch, parseDiffFromFile } from "@pierre/diffs";
import { isDarkModeActive } from "../../../services/theme";
import { useFileDiffInstance } from "../../composables/fileDiffInstance";
import { useNearViewport } from "../../composables/nearViewport";

// LocalStorage key for side-by-side preference
const STORAGE_KEY_SIDE_BY_SIDE = "shelley-diff-side-by-side";

function getSideBySidePreference(): boolean {
  try {
    const stored = localStorage.getItem(STORAGE_KEY_SIDE_BY_SIDE);
    if (stored !== null) {
      return stored === "true";
    }
    return window.innerWidth >= 768;
  } catch {
    return window.innerWidth >= 768;
  }
}

function setSideBySidePreference(value: boolean): void {
  try {
    localStorage.setItem(STORAGE_KEY_SIDE_BY_SIDE, value ? "true" : "false");
  } catch {
    // Ignore storage errors
  }
}

interface PatchDisplayData {
  path: string;
  diff?: string;
  oldContent?: string;
  newContent?: string;
}

const DIFF_THEMES: ThemesType = { dark: "github-dark", light: "github-light" };

// Map file extension to language for syntax highlighting
function getLanguageFromPath(path: string): SupportedLanguages {
  const ext = path.split(".").pop()?.toLowerCase() || "";
  const langMap: Record<string, SupportedLanguages> = {
    ts: "typescript",
    tsx: "tsx",
    js: "javascript",
    jsx: "jsx",
    py: "python",
    rb: "ruby",
    go: "go",
    rs: "rust",
    java: "java",
    c: "c",
    cpp: "cpp",
    h: "c",
    hpp: "cpp",
    cs: "csharp",
    php: "php",
    swift: "swift",
    kt: "kotlin",
    scala: "scala",
    sh: "bash",
    bash: "bash",
    zsh: "bash",
    fish: "fish",
    ps1: "powershell",
    sql: "sql",
    html: "html",
    htm: "html",
    css: "css",
    scss: "scss",
    sass: "sass",
    less: "less",
    json: "json",
    xml: "xml",
    yaml: "yaml",
    yml: "yaml",
    toml: "toml",
    ini: "ini",
    md: "markdown",
    markdown: "markdown",
    txt: "text",
    dockerfile: "dockerfile",
    makefile: "makefile",
    cmake: "cmake",
    lua: "lua",
    perl: "perl",
    r: "r",
    vue: "vue",
    svelte: "svelte",
    astro: "astro",
  };
  return langMap[ext] || "text";
}

const props = defineProps<{
  toolInput?: unknown;
  isRunning?: boolean;
  toolResult?: LLMContent[];
  hasError?: boolean;
  executionTime?: string;
  display?: unknown;
  onCommentTextChange?: (text: string) => void;
}>();

// State
const isExpanded = ref(!props.hasError);
const isMobile = ref(window.innerWidth < 768);
const sideBySide = ref(!isMobile.value && getSideBySidePreference());
// Host element for the FileDiff renderer's <diffs-container>.
const diffHostEl = ref<HTMLElement | null>(null);
// Whether this tool has ever been near the viewport. Diff parsing + FileDiff
// hydration are deferred until then: hundreds of off-screen hydrated diffs
// otherwise dominate whole-page layout (window resizes).
const nearViewport = useNearViewport(diffHostEl);

// Reactive theme tracking (mirrors the React useThemeType hook)
const themeType = ref<ThemeTypes>(isDarkModeActive() ? "dark" : "light");

let themeObserver: MutationObserver | null = null;
onMounted(() => {
  themeObserver = new MutationObserver((mutations) => {
    for (const mutation of mutations) {
      if (mutation.attributeName === "class") {
        themeType.value = isDarkModeActive() ? "dark" : "light";
      }
    }
  });
  themeObserver.observe(document.documentElement, { attributes: true });
});
onUnmounted(() => {
  themeObserver?.disconnect();
});

// Viewport resize handler
function handleResize() {
  const mobile = window.innerWidth < 768;
  isMobile.value = mobile;
  if (mobile) {
    sideBySide.value = false;
  }
}

onMounted(() => {
  window.addEventListener("resize", handleResize);
});
onUnmounted(() => {
  window.removeEventListener("resize", handleResize);
});

function toggleSideBySide() {
  const newValue = !sideBySide.value;
  sideBySide.value = newValue;
  setSideBySidePreference(newValue);
}

// Computed properties
const path = computed(() => {
  const ti = props.toolInput;
  if (
    typeof ti === "object" &&
    ti !== null &&
    "path" in ti &&
    typeof (ti as { path: unknown }).path === "string"
  ) {
    return (ti as { path: string }).path;
  }
  return typeof ti === "string" ? ti : "";
});

const displayData = computed<PatchDisplayData | null>(() => {
  const d = props.display;
  if (d && typeof d === "object" && "path" in d) {
    return d as PatchDisplayData;
  }
  return null;
});

const errorMessage = computed(() =>
  props.toolResult && props.toolResult.length > 0 && props.toolResult[0].Text
    ? props.toolResult[0].Text
    : "",
);

const isComplete = computed(() => !props.isRunning && props.toolResult !== undefined);

const hasDiff = computed(
  () =>
    displayData.value != null &&
    (displayData.value.diff ||
      (displayData.value.oldContent != null && displayData.value.newContent != null)),
);

const filename = computed(() => displayData.value?.path || path.value || "patch");

const showDiffToggle = computed(
  () => !isMobile.value && isExpanded.value && isComplete.value && !props.hasError && hasDiff.value,
);

// Raw diff string for fallback display
const rawDiff = computed(() => {
  if (!displayData.value) return "";
  return displayData.value.diff ?? "";
});

// FileDiff render options derived from the current side-by-side + theme state.
const diffOptions = computed<FileDiffOptions<undefined>>(() => ({
  diffStyle: sideBySide.value ? "split" : "unified",
  theme: DIFF_THEMES,
  themeType: themeType.value,
  disableFileHeader: true,
}));

// Parse the diff into FileDiff metadata. getSingularPatch handles unified diff
// strings; parseDiffFromFile handles legacy old/new file-content snapshots
// (immune to content that looks like diff headers, mirroring React's
// MultiFileDiff path). Returns null when there's nothing renderable or parsing
// fails (the template falls back to the raw <pre>).
const fileDiff = computed<FileDiffMetadata | null>(() => {
  const dd = displayData.value;
  if (!dd) return null;
  try {
    if (dd.oldContent != null && dd.newContent != null) {
      const lang = getLanguageFromPath(dd.path);
      const oldFile: FileContents = { name: dd.path, contents: dd.oldContent, lang };
      const newFile: FileContents = { name: dd.path, contents: dd.newContent, lang };
      return parseDiffFromFile(oldFile, newFile);
    }
    if (dd.diff) {
      return getSingularPatch(dd.diff);
    }
  } catch (e) {
    console.warn("PatchTool diff parse error:", e);
  }
  return null;
});

// True when we have a diff to show but couldn't parse it into renderable
// metadata — the template then falls back to the raw <pre>. Gated on
// nearViewport so the parse itself is deferred with hydration. Derived (no
// side effects) so it stays correct as inputs change.
const diffError = computed(() => nearViewport.value && hasDiff.value && fileDiff.value == null);

// Rough height reserved for a not-yet-hydrated diff so scrolling up through
// history doesn't shift as diffs hydrate. ~20px per diff line; snapshots
// (old/new content) render only changed hunks, so estimate conservatively.
const placeholderHeight = computed(() => {
  const dd = displayData.value;
  if (!dd) return "0";
  let lines = 4;
  if (dd.diff) {
    lines = dd.diff.split("\n").length;
  } else if (dd.newContent != null && dd.oldContent != null) {
    lines = Math.min(
      Math.abs(dd.newContent.split("\n").length - dd.oldContent.split("\n").length) + 8,
      80,
    );
  }
  return `${Math.min(lines * 20, 2000)}px`;
});

// Where this tool renders relative to the viewport: see nearViewport above.

// Drive the FileDiff renderer (off-main-thread tokenization via the shared
// worker pool) whenever the parsed diff + options are ready and the section is
// visible. Returns null inputs (teardown) when the section is collapsed/errored
// so we don't render hidden diffs.
const { rendered } = useFileDiffInstance(diffHostEl, () => {
  if (
    !nearViewport.value ||
    !isExpanded.value ||
    !isComplete.value ||
    props.hasError ||
    !hasDiff.value
  ) {
    return null;
  }
  const fd = fileDiff.value;
  if (!fd) return null;
  return { fileDiff: fd, options: diffOptions.value };
});
</script>
