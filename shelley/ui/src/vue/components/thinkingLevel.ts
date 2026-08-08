// Shared thinking-level constants/types. "default" leaves the request unset so
// the selected model's configured/provider default applies.
export type ThinkingLevel = "default" | "off" | "minimal" | "low" | "medium" | "high" | "xhigh";

export const DEFAULT_THINKING_LEVEL: ThinkingLevel = "default";

export const THINKING_LEVELS: { value: ThinkingLevel; label: string }[] = [
  { value: "default", label: "default" },
  { value: "off", label: "off" },
  { value: "minimal", label: "minimal" },
  { value: "low", label: "low" },
  { value: "medium", label: "medium" },
  { value: "high", label: "high" },
  { value: "xhigh", label: "xhigh" },
];
