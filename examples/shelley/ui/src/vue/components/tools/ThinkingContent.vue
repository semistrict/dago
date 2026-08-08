<!-- Vue port of components/ThinkingContent.tsx. Collapsible chain-of-thought,
     default collapsed. Preserves: .thinking-content, .thinking-content-wrapper,
     data-testid thinking-content, .thinking-clickable-area, .thinking-emoji 💭,
     .thinking-text, .thinking-toggle, .thinking-toggle-button. -->
<template>
  <div class="thinking-content thinking-content-wrapper" data-testid="thinking-content">
    <div class="thinking-clickable-area" @click="isExpanded = !isExpanded">
      <span class="thinking-emoji">💭</span>
      <div class="thinking-text" :class="{ 'thinking-text-collapsed': !isExpanded }">
        {{ isExpanded ? thinking : preview }}
      </div>
      <button
        class="thinking-toggle thinking-toggle-button"
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
</template>

<script setup lang="ts">
import { computed, ref } from "vue";

const props = defineProps<{ thinking: string }>();

const isExpanded = ref(false);

// Collapsed preview: first line only, capped to keep the DOM light.
// Visual truncation (ellipsis at the edge of the line) is done in CSS
// via .thinking-text-collapsed.
const preview = computed(() => {
  if (!props.thinking) return "";
  const firstLine = props.thinking.split("\n", 1)[0];
  return firstLine.length > 500 ? firstLine.substring(0, 500) : firstLine;
});
</script>
