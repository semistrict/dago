<!-- Right-hand status-bar readout: "~/dir · 15k · model-name".

     Three segments, dot-separated, sharing one type style (set here, inherited
     by the segments, so they can't drift apart the way they did when each
     declared its own). Two of them are controls with distinct destinations:
     the token count opens the context/cost popup, the model name opens the
     model picker. The cwd is the only plain text.

     Rendered for a conversation that already exists — both while it is idle and
     while the agent works — so the model here is server state and a pick has to
     go through the server (see onSwitchConversationModel). The composer's boxed
     ModelPicker covers the pre-first-send case instead. -->
<template>
  <div class="status-readout">
    <template v-if="cwd">
      <span class="status-readout-cwd hide-on-mobile" :title="cwd">{{ tildifyPath(cwd) }}</span>
      <span class="status-readout-sep hide-on-mobile" aria-hidden="true">·</span>
    </template>

    <ContextUsageBar
      :context-window-size="contextWindowSize"
      :max-context-tokens="maxContextTokens"
      :conversation-id="conversationId"
      :usage-entries="usageEntries"
      :other-usage-rows="otherUsageRows"
      :on-distill-new-generation="onDistillNewGeneration"
      :on-start-new-generation="onStartNewGeneration"
      :on-usage-needed="onUsageNeeded"
      :agent-working="agentWorking"
    />

    <template v-if="selectedModel">
      <span class="status-readout-sep" aria-hidden="true">·</span>
      <!-- Switching model rebuilds the conversation's loop, which cancels a
           running turn (ApplyModelSettings -> CancelConversation). Disable the
           picker while the agent works rather than silently killing the turn the
           user is watching, and say why two ways: the tooltip has to hang off
           this wrapper (PrimeVue gives a disabled control pointer-events: none,
           so nothing on the picker itself would ever fire), while the ARIA
           description goes on the combobox, which is what a screen reader
           actually lands on. No `title` alongside the tooltip — that renders
           both the PrimeVue bubble and the browser's native one. -->
      <span v-tooltip.top="busyReason" class="status-readout-model">
        <ModelPicker
          inline
          :disabled-reason="busyReason"
          :models="models"
          :selected-model="selectedModel"
          :thinking-level="thinkingLevel"
          :disabled="agentWorking"
          :refreshing="refreshingModels"
          @select-model="onSwitchConversationModel"
          @thinking-change="onSwitchConversationThinkingLevel"
          @manage-models="onManageModels"
          @refresh-models="onRefreshModels"
        />
      </span>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed } from "vue";
import type { Model } from "../../types";
import type { OtherUsageRow, UsageEntry } from "../../utils/tokenCostGraph";
import { tildifyPath } from "../../utils/tildify";
import { useI18n } from "../composables/i18n";
import type { ThinkingLevel } from "./thinkingLevel";
import ContextUsageBar from "./ContextUsageBar.vue";
import ModelPicker from "./ModelPicker.vue";

const props = defineProps<{
  cwd?: string;
  conversationId?: string | null;
  contextWindowSize: number;
  maxContextTokens: number;
  usageEntries?: UsageEntry[];
  otherUsageRows?: OtherUsageRow[];
  models: Model[];
  selectedModel: string;
  thinkingLevel: ThinkingLevel;
  refreshingModels?: boolean;
  agentWorking?: boolean;
  onDistillNewGeneration?: () => Promise<void> | void;
  onStartNewGeneration?: () => Promise<void> | void;
  onUsageNeeded?: () => void;
  onSwitchConversationModel: (model: string) => void;
  onSwitchConversationThinkingLevel: (level: ThinkingLevel) => void;
  onManageModels: () => void;
  onRefreshModels: () => void;
}>();

const { t } = useI18n();

// Undefined rather than "" when idle: v-tooltip treats an empty string as a
// tooltip to render, and the ModelPicker prop is optional.
const busyReason = computed(() => (props.agentWorking ? t("modelSwitchBusy") : undefined));
</script>
