import type { TranslationKeys } from "../i18n/types";

export type TranslationKey = keyof TranslationKeys;

export interface ModelSetupHintCopy {
  title: TranslationKey;
  note?: TranslationKey;
  actions: [];
}

const LOCAL: ModelSetupHintCopy = {
  title: "noModelsTitle",
  note: "noModelsLocalNote",
  actions: [],
};

export function modelSetupHintKeys(hint?: string): ModelSetupHintCopy {
  void hint;
  return LOCAL;
}

export function canSendWithModel(
  model: string | undefined,
  readyModelIds?: readonly string[],
): boolean {
  if (!model || model.trim() === "") return false;
  if (!readyModelIds) return true;
  return readyModelIds.includes(model);
}

const MODEL_FREE_EXACT = ["/fork", "/diff", "/archive", "/clear"];
const MODEL_FREE_PREFIX = ["/rename"];

export function needsModel(message: string): boolean {
  const trimmed = message.trim();
  if (trimmed.startsWith("!")) return false;
  if (MODEL_FREE_EXACT.includes(trimmed)) return false;
  for (const command of MODEL_FREE_PREFIX) {
    if (trimmed === command || trimmed.startsWith(`${command} `)) return false;
  }
  return true;
}
