// Shared constants + form types for the custom-model UI, used by both
// ModelsModal.vue (the list) and ModelFormModal.vue (the add/edit dialog).

export type ProviderType = "openai-responses";

export const DEFAULT_ENDPOINTS: Record<ProviderType, string> = {
  "openai-responses": "https://api.openai.com/v1",
};

export const PROVIDER_LABELS: Record<ProviderType, string> = {
  "openai-responses": "OpenAI (Responses API)",
};

// Autocomplete suggestions offered for the model-name field, per provider.
export const DEFAULT_MODELS: Record<ProviderType, { name: string; model_name: string }[]> = {
  "openai-responses": [
    { name: "GPT-5.6 Sol", model_name: "gpt-5.6-sol" },
    { name: "GPT-5.6 Terra", model_name: "gpt-5.6-terra" },
    { name: "GPT-5.6 Luna", model_name: "gpt-5.6-luna" },
    { name: "GPT-5.5", model_name: "gpt-5.5" },
    { name: "GPT-5.4", model_name: "gpt-5.4" },
    { name: "GPT-5.4 mini", model_name: "gpt-5.4-mini" },
    { name: "GPT-5.3 Codex", model_name: "gpt-5.3-codex" },
  ],
};

// Maps the server-reported api_type of built-in models to a display label.
export const API_TYPE_LABELS: Record<string, string> = {
  "openai-responses": "OpenAI (Responses API)",
  builtin: "Built-in",
};

export const REASONING_EFFORT_SUGGESTIONS = ["none", "minimal", "low", "medium", "high", "xhigh"];

export const REASONING_LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh"] as const;
export type ReasoningLevel = (typeof REASONING_LEVELS)[number];
export type ReasoningMap = Record<ReasoningLevel, string>;
export const DEFAULT_REASONING_MAP: ReasoningMap = Object.fromEntries(
  REASONING_LEVELS.map((level) => [level, level]),
) as ReasoningMap;

export const providerTypes: ProviderType[] = ["openai-responses"];

export interface FormData {
  display_name: string;
  provider_type: ProviderType;
  endpoint: string;
  endpoint_custom: boolean;
  api_key: string;
  model_name: string;
  max_tokens: number;
  tags: string;
  reasoning_effort: string;
  reasoning_support: "auto" | "yes" | "no";
  reasoning_map: ReasoningMap;
  image_support: "auto" | "yes" | "no";
}

export const emptyForm: FormData = {
  display_name: "",
  provider_type: "openai-responses",
  endpoint: DEFAULT_ENDPOINTS["openai-responses"],
  endpoint_custom: false,
  api_key: "",
  model_name: "",
  max_tokens: 200000,
  tags: "",
  reasoning_effort: "",
  reasoning_support: "auto",
  reasoning_map: { ...DEFAULT_REASONING_MAP },
  image_support: "auto",
};
