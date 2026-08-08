import type { Conversation } from "../types";

type DrawerConversation = Pick<Conversation, "parent_conversation_id">;

const COLLAPSED_KEY = "shelley-drawer-collapsed";

export function shouldStartDrawerCollapsed(conversations: DrawerConversation[]): boolean {
  let topLevelCount = 0;
  for (const conversation of conversations) {
    if (conversation.parent_conversation_id) continue;
    topLevelCount += 1;
    if (topLevelCount > 1) return false;
  }
  return true;
}

// initialDrawerCollapsed decides the drawer's startup state: a preference
// saved by the user's last manual toggle wins; otherwise fall back to the
// sparse-drawer heuristic above.
export function initialDrawerCollapsed(
  conversations: DrawerConversation[],
  storage: Storage,
): boolean {
  try {
    const saved = storage.getItem(COLLAPSED_KEY);
    if (saved === "true") return true;
    if (saved === "false") return false;
  } catch {
    // Storage unavailable (e.g. disabled); use the heuristic.
  }
  return shouldStartDrawerCollapsed(conversations);
}

export function saveDrawerCollapsedPreference(collapsed: boolean, storage: Storage): void {
  try {
    storage.setItem(COLLAPSED_KEY, String(collapsed));
  } catch {
    // Best effort; losing the preference is fine.
  }
}
