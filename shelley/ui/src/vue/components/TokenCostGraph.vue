<!-- Stacked cumulative token-cost graph shown in the context usage popup.
     X axis: LLM calls or wall-clock time (toggle); in time mode, idle time
     between turns is collapsed into fixed-width gaps. Y axis: cumulative
     dollars per (model, token-band) segment — every segment has its own color
     — falling back to raw token counts when no model in the conversation has
     known pricing.

     Subagent cost and "other" (indirect) LLM usage — compaction
     summarization, LLM-backed tools, slug generation, … — are not part of the
     graph. Other-usage rows arrive via the otherUsageRows prop (aggregated
     client-side from message other_usage_data) and subagent cost is fetched
     separately; both show up as an "Other (indirect)" breakdown section and
     "plus ≈$Y other, plus ≈$Z for subagents" note segments. -->
<template>
  <div class="token-cost-graph">
    <div v-if="loading" class="token-cost-graph-note">Loading pricing…</div>
    <template v-else-if="stack && stack.n > 0">
      <div class="token-cost-controls">
        <button
          :class="{ 'token-cost-toggle-active': xMode === 'calls' }"
          class="token-cost-toggle"
          @click="xMode = 'calls'"
        >
          calls
        </button>
        <button
          :class="{ 'token-cost-toggle-active': xMode === 'time' }"
          class="token-cost-toggle"
          @click="xMode = 'time'"
        >
          time
        </button>
      </div>
      <svg
        :viewBox="`0 0 ${W} ${H}`"
        class="token-cost-graph-svg"
        @mousemove="onMove"
        @mouseleave="hoverIndex = null"
      >
        <line
          v-for="t in ticks"
          :key="`tick-${t}`"
          :x1="PADL"
          :y1="yAt(t)"
          :x2="W - PADR"
          :y2="yAt(t)"
          class="token-cost-gridline"
        />
        <path v-for="(d, s) in segPaths" :key="s" :d="d" :fill="stack.segments[s].color" />
        <line
          v-for="i in genStarts"
          :key="`gen-${i}`"
          :x1="xAt(i)"
          :y1="PADT"
          :x2="xAt(i)"
          :y2="H - PADB"
          class="token-cost-gen-line"
        />
        <line :x1="PADL" :y1="PADT" :x2="PADL" :y2="H - PADB" class="token-cost-axis" />
        <line :x1="PADL" :y1="H - PADB" :x2="W - PADR" :y2="H - PADB" class="token-cost-axis" />
        <line
          v-if="hoverIndex !== null && stack.n > 1"
          :x1="xAt(hoverIndex)"
          :y1="PADT"
          :x2="xAt(hoverIndex)"
          :y2="H - PADB"
          class="token-cost-hover-line"
        />
        <text
          v-for="t in ticks"
          :key="`ticklabel-${t}`"
          :x="PADL - 3"
          :y="yAt(t) + 3"
          text-anchor="end"
          class="token-cost-label"
        >
          {{ tickLabel(t) }}
        </text>
        <text :x="(PADL + W - PADR) / 2" :y="H - 4" text-anchor="middle" class="token-cost-label">
          {{ xAxisLabel }}
        </text>
      </svg>
      <div class="token-cost-hover-readout">
        <template v-if="hoverEntry">
          <div>
            call {{ hoverIndex! + 1 }} of {{ stack.n
            }}<template v-if="hoverGeneration"> (gen {{ hoverGeneration }})</template
            ><template v-if="hoverTime"> · {{ hoverTime }}</template> · cumulative {{ hoverTotal }}
          </div>
          <div v-if="hoverEntry.snippet" class="token-cost-hover-snippet">
            {{ hoverEntry.snippet }}
          </div>
        </template>
        <template v-else>
          <div>{{ hintText }}</div>
        </template>
      </div>
      <div class="token-cost-legend">
        <template v-for="mu in stack.perModel" :key="mu.model">
          <div class="token-cost-model-row">
            <span class="token-cost-model-name">{{ mu.model }}</span>
            <span v-if="mu.priced" class="token-cost-legend-cost">{{
              formatUsd(mu.totalCost)
            }}</span>
            <span v-else-if="mu.reportedUsd > 0" class="token-cost-legend-cost">
              {{ formatUsd(mu.reportedUsd) }} reported
            </span>
            <span v-else class="token-cost-legend-unit">no pricing</span>
          </div>
          <div v-for="t in rowsFor(mu)" :key="t.band.key" class="token-cost-legend-row">
            <span class="token-cost-chip" :style="{ backgroundColor: t.color }" />
            <span class="token-cost-legend-label">{{ t.band.label }}</span>
            <span class="token-cost-legend-tokens">{{ formatTokenCount(t.tokens) }}</span>
            <span v-if="mu.priced" class="token-cost-legend-unit">
              @ {{ formatUnitPrice(t.unitUsdPerMtok) }}
            </span>
            <span v-if="mu.priced" class="token-cost-legend-cost">{{ formatUsd(t.cost) }}</span>
          </div>
        </template>
        <template v-if="otherBreakdown && otherBreakdown.perPurpose.length > 0">
          <div class="token-cost-model-row">
            <span class="token-cost-model-name">Other (indirect)</span>
            <span v-if="otherBreakdown.totals.estimatedUsd > 0" class="token-cost-legend-cost">{{
              formatUsd(otherBreakdown.totals.estimatedUsd)
            }}</span>
            <span
              v-else-if="otherBreakdown.totals.reportedUsd > 0"
              class="token-cost-legend-cost"
            >
              {{ formatUsd(otherBreakdown.totals.reportedUsd) }} reported
            </span>
            <span
              v-else-if="otherBreakdown.totals.unpricedCalls === 0"
              class="token-cost-legend-cost"
              >{{ formatUsd(0) }}</span
            >
            <span v-else class="token-cost-legend-unit">no pricing</span>
          </div>
          <div
            v-for="p in otherBreakdown.perPurpose"
            :key="p.purpose"
            class="token-cost-legend-row"
          >
            <span class="token-cost-legend-label">{{ p.purpose }}</span>
            <span class="token-cost-legend-tokens"
              >{{ p.llmCalls }} {{ p.llmCalls === 1 ? "call" : "calls" }}</span
            >
            <span class="token-cost-legend-tokens">{{ formatTokenCount(p.tokens) }}</span>
            <span v-if="p.priced" class="token-cost-legend-cost">{{
              formatUsd(p.estimatedUsd)
            }}</span>
            <span v-else-if="p.reportedUsd > 0" class="token-cost-legend-cost">
              {{ formatUsd(p.reportedUsd) }} reported
            </span>
            <span v-else class="token-cost-legend-unit">no pricing</span>
          </div>
        </template>
      </div>
      <div class="token-cost-graph-note">
        <template v-if="fetchFailed && !stack.weighted">
          Pricing lookup failed — showing raw token counts.
        </template>
        <template v-else-if="stack.weighted">
          ≈{{ formatUsd(stack.maxY) }} total<template v-if="otherNote">, {{ otherNote }}</template
          ><template v-if="subagentNote">, {{ subagentNote }}</template>
          <template v-if="fetchFailed"> · pricing lookup failed for some models</template>
          <template v-if="stack.reportedCostUsd > 0">
            · provider-reported {{ formatUsd(stack.reportedCostUsd) }}
          </template>
        </template>
        <template v-else
          >Raw token counts — no pricing known.<template v-if="stack.reportedCostUsd > 0">
            Provider-reported {{ formatUsd(stack.reportedCostUsd) }}.</template
          ></template
        >
      </div>
      <div v-if="!stack.weighted && otherNote" class="token-cost-graph-note">
        Other: {{ otherNote }}
      </div>
      <div v-if="!stack.weighted && subagentNote" class="token-cost-graph-note">
        Subagents: {{ subagentNote }}
      </div>
    </template>
    <div v-else class="token-cost-graph-note">No usage data yet.</div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from "vue";
import {
  modelCostsApi,
  subagentUsageApi,
  type ModelCostDTO,
  type SubagentUsageDTO,
} from "../../services/api";
import {
  buildOtherUsageBreakdown,
  buildTokenCostStack,
  callXLayout,
  formatDuration,
  formatTokenCount,
  formatUsd,
  generationStarts,
  timeXLayout,
  yTicks,
  type ModelUsage,
  type OtherUsageBreakdown,
  type OtherUsageRow,
  type TokenCostStack,
  type UsageEntry,
  type XLayout,
} from "../../utils/tokenCostGraph";

const props = defineProps<{
  entries: UsageEntry[];
  otherUsageRows?: OtherUsageRow[];
  conversationId?: string | null;
}>();

const W = 280;
const H = 150;
const PADL = 32;
const PADR = 6;
const PADT = 6;
const PADB = 18;

const loading = ref(false);
const fetchFailed = ref(false);
const costs = ref<Record<string, ModelCostDTO | null>>({});

// "Other" (indirect) LLM usage — compaction, LLM-backed tools, slug
// generation, … — arrives pre-aggregated via the otherUsageRows prop. It has
// no per-call timeline, so it stays out of the graph and only appears in the
// breakdown and the note line; its models join the pricing batch below.

// (model, url) pairs to price: the graph's entries plus other-usage models,
// deduped by model name (first-seen URL wins).
const distinctModels = computed(() => {
  const seen = new Map<string, string>();
  for (const e of props.entries) {
    if (e.model && !seen.has(e.model)) seen.set(e.model, e.url || "");
  }
  for (const row of props.otherUsageRows ?? []) {
    if (row.model && !seen.has(row.model)) seen.set(row.model, row.url || "");
  }
  return seen;
});

watch(
  distinctModels,
  async (models) => {
    if (models.size === 0) return;
    loading.value = Object.keys(costs.value).length === 0;
    try {
      costs.value = await modelCostsApi.lookup(
        Array.from(models).map(([model, url]) => ({ model, url })),
      );
      fetchFailed.value = false;
    } catch (e) {
      console.warn("model costs lookup failed", e);
      fetchFailed.value = true;
    } finally {
      loading.value = false;
    }
  },
  { immediate: true },
);

const stack = computed<TokenCostStack | null>(() =>
  props.entries.length > 0 ? buildTokenCostStack(props.entries, costs.value) : null,
);

// Subagent usage is aggregated server-side (a recursive query over descendant
// conversations) and shown as a separate note line, not in the graph.
const subagentUsage = ref<SubagentUsageDTO | null>(null);
watch(
  () => props.conversationId,
  async (id) => {
    subagentUsage.value = null;
    if (!id) return;
    try {
      subagentUsage.value = await subagentUsageApi.get(id);
    } catch (e) {
      console.warn("subagent usage lookup failed", e);
    }
  },
  { immediate: true },
);

const subagentNote = computed(() => {
  const sub = subagentUsage.value;
  if (!sub || sub.llm_calls === 0) return "";
  if (sub.estimated_usd > 0) {
    let note = `plus ≈${formatUsd(sub.estimated_usd)} for subagents`;
    if (sub.unpriced_calls > 0) note += ` (${sub.unpriced_calls} calls unpriced)`;
    return note;
  }
  return `plus ${sub.llm_calls} unpriced subagent calls`;
});

const otherBreakdown = computed<OtherUsageBreakdown | null>(() => {
  const rows = props.otherUsageRows;
  if (!rows || rows.length === 0) return null;
  return buildOtherUsageBreakdown(rows, costs.value);
});

const otherNote = computed(() => {
  const b = otherBreakdown.value;
  if (!b || b.totals.llmCalls === 0) return "";
  if (b.totals.estimatedUsd > 0) {
    let note = `plus ≈${formatUsd(b.totals.estimatedUsd)} other`;
    if (b.totals.unpricedCalls > 0) note += ` (${b.totals.unpricedCalls} calls unpriced)`;
    return note;
  }
  if (b.totals.reportedUsd > 0) {
    return `plus ${formatUsd(b.totals.reportedUsd)} other (reported)`;
  }
  return `plus ${b.totals.llmCalls} unpriced other calls`;
});

const plotW = W - PADL - PADR;
const plotH = H - PADT - PADB;

const xMode = ref<"calls" | "time">("calls");

const layout = computed<XLayout>(() =>
  xMode.value === "time" ? timeXLayout(props.entries) : callXLayout(stack.value?.n ?? 0),
);

function xAt(i: number): number {
  return PADL + layout.value.xs[i] * plotW;
}

function yAt(v: number): number {
  const maxY = stack.value?.maxY || 1;
  return PADT + plotH * (1 - v / maxY);
}

// One path per (model, band) segment; each turn is a separate subpath so
// idle time between turns renders as a gap in time mode. Zero-width turns
// (single call, or all calls sharing one second-granularity timestamp)
// become narrow slabs so they stay visible.
const segPaths = computed<string[]>(() => {
  const s = stack.value;
  const lay = layout.value;
  if (!s || s.n === 0 || s.maxY === 0) return [];
  const px = (i: number) => (PADL + lay.xs[i] * plotW).toFixed(1);
  const lower = (si: number, i: number) => (si === 0 ? 0 : s.layers[si - 1][i]);
  return s.segments.map((_, si) => {
    let d = "";
    for (const [a, b] of lay.turns) {
      if (lay.xs[a] === lay.xs[b]) {
        const x = PADL + lay.xs[a] * plotW;
        const hw = Math.max(1, plotW * 0.006);
        const x0 = Math.max(PADL, x - hw).toFixed(1);
        const x1 = Math.min(W - PADR, x + hw).toFixed(1);
        const yT = yAt(s.layers[si][b]).toFixed(1);
        const yB = yAt(lower(si, b)).toFixed(1);
        d += `M${x0},${yT}L${x1},${yT}L${x1},${yB}L${x0},${yB}Z`;
        continue;
      }
      const top: string[] = [];
      for (let i = a; i <= b; i++) top.push(`${px(i)},${yAt(s.layers[si][i]).toFixed(1)}`);
      const bottom: string[] = [];
      for (let i = b; i >= a; i--) bottom.push(`${px(i)},${yAt(lower(si, i)).toFixed(1)}`);
      d += `M${top.join("L")}L${bottom.join("L")}Z`;
    }
    return d;
  });
});

const ticks = computed<number[]>(() => yTicks(stack.value?.maxY ?? 0));

function tickLabel(v: number): string {
  if (!stack.value?.weighted) return formatTokenCount(v);
  if (v >= 10) return `$${Math.round(v)}`;
  if (v >= 1) return `$${parseFloat(v.toFixed(1))}`;
  if (v >= 0.01) return `$${parseFloat(v.toFixed(2))}`;
  return formatUsd(v);
}

const xAxisLabel = computed(() => {
  const s = stack.value;
  if (!s) return "";
  if (xMode.value === "calls") return `LLM call number (${s.n} calls)`;
  const lay = layout.value;
  const dur = lay.activeMs > 0 ? `${formatDuration(lay.activeMs)} active` : "time";
  return lay.turns.length > 1 ? `${dur} · gaps = idle between turns` : dur;
});

const hintText = computed(() => {
  const parts: string[] = [];
  if (genStarts.value.length) parts.push("Dashed lines mark new generations (compactions).");
  if (xMode.value === "time" && layout.value.turns.length > 1)
    parts.push("Idle time between turns is not to scale.");
  return parts.join(" ");
});

// Rows top-to-bottom mirror the band stacking order within the model.
function rowsFor(mu: ModelUsage) {
  return [...mu.rows].reverse();
}

/** "$50/M", "$12.50/M", "$0.008/M" — unit price per million tokens. */
function formatUnitPrice(usdPerMtok: number): string {
  let s: string;
  if (Number.isInteger(usdPerMtok)) s = String(usdPerMtok);
  else if (usdPerMtok >= 0.1) s = usdPerMtok.toFixed(2);
  else s = usdPerMtok.toPrecision(2).replace(/\.?0+$/, "");
  return `$${s}/M`;
}

const hoverIndex = ref<number | null>(null);

// A shrinking entries list (e.g. switching conversations) could leave a stale
// out-of-range index behind.
watch(
  () => stack.value?.n,
  () => (hoverIndex.value = null),
);

function onMove(ev: MouseEvent) {
  const s = stack.value;
  if (!s || s.n === 0) return;
  const svg = ev.currentTarget as SVGSVGElement;
  const rect = svg.getBoundingClientRect();
  const px = ((ev.clientX - rect.left) / rect.width) * W;
  const frac = (px - PADL) / plotW;
  const xs = layout.value.xs;
  let best = 0;
  let bestD = Infinity;
  for (let i = 0; i < xs.length; i++) {
    const d = Math.abs(xs[i] - frac);
    if (d < bestD) {
      bestD = d;
      best = i;
    }
  }
  hoverIndex.value = best;
}

const hoverEntry = computed<UsageEntry | null>(() => {
  const s = stack.value;
  const i = hoverIndex.value;
  if (!s || i === null || i >= s.n) return null;
  return props.entries[i];
});

const genStarts = computed(() => generationStarts(props.entries));

// Generation number shown in the hover readout, only when the conversation
// actually spans multiple generations.
const hoverGeneration = computed<number | null>(() => {
  if (genStarts.value.length === 0) return null;
  return hoverEntry.value?.generation ?? null;
});

// Wall-clock time of the hovered call, shown in time mode.
const hoverTime = computed(() => {
  const ts = hoverEntry.value?.timestamp;
  if (xMode.value !== "time" || !ts) return "";
  return new Date(ts).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
});

const hoverTotal = computed(() => {
  const s = stack.value;
  const i = hoverIndex.value;
  if (!s || i === null || i >= s.n || s.segments.length === 0) return "";
  const top = s.layers[s.segments.length - 1][i];
  return s.weighted ? formatUsd(top) : `${formatTokenCount(top)} tok`;
});
</script>
