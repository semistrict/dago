import type { Conversation } from "../types";
import {
  initialDrawerCollapsed,
  saveDrawerCollapsedPreference,
  shouldStartDrawerCollapsed,
} from "./drawerStartup";

function assert(cond: boolean, msg: string): void {
  if (!cond) throw new Error(`Assertion failed: ${msg}`);
}

function run(name: string, fn: () => void): void {
  try {
    fn();
    console.log(`\u2713 ${name}`);
  } catch (err) {
    console.error(`\u2717 ${name}`);
    throw err;
  }
}

function conversation(
  id: string,
  options: { parentId?: string | null; isDraft?: boolean } = {},
): Pick<Conversation, "conversation_id" | "parent_conversation_id" | "is_draft"> {
  return {
    conversation_id: id,
    parent_conversation_id: options.parentId ?? null,
    is_draft: options.isDraft ?? false,
  };
}

run("starts collapsed with no saved conversations", () => {
  assert(shouldStartDrawerCollapsed([]), "empty drawer should start collapsed");
});

run("starts collapsed with one conversation", () => {
  assert(
    shouldStartDrawerCollapsed([conversation("only")]),
    "single conversation should start collapsed",
  );
});

run("starts collapsed with only a draft", () => {
  assert(
    shouldStartDrawerCollapsed([conversation("draft", { isDraft: true })]),
    "single draft should start collapsed",
  );
});

run("ignores subagents when deciding whether the drawer is useful", () => {
  assert(
    shouldStartDrawerCollapsed([
      conversation("parent"),
      conversation("subagent", { parentId: "parent" }),
    ]),
    "one top-level conversation plus a subagent should start collapsed",
  );
});

run("starts expanded with multiple top-level conversations", () => {
  assert(
    !shouldStartDrawerCollapsed([conversation("first"), conversation("second")]),
    "multiple conversations should start expanded",
  );
});

function fakeStorage(initial: Record<string, string> = {}): Storage {
  const data = new Map(Object.entries(initial));
  return {
    getItem: (key: string) => data.get(key) ?? null,
    setItem: (key: string, value: string) => void data.set(key, value),
    removeItem: (key: string) => void data.delete(key),
    clear: () => data.clear(),
    key: (index: number) => [...data.keys()][index] ?? null,
    get length() {
      return data.size;
    },
  };
}

run("falls back to the heuristic without a saved preference", () => {
  const storage = fakeStorage();
  assert(
    initialDrawerCollapsed([conversation("only")], storage),
    "single conversation should start collapsed without a preference",
  );
  assert(
    !initialDrawerCollapsed([conversation("first"), conversation("second")], storage),
    "multiple conversations should start expanded without a preference",
  );
});

run("a saved expanded preference overrides the heuristic", () => {
  const storage = fakeStorage();
  saveDrawerCollapsedPreference(false, storage);
  assert(
    !initialDrawerCollapsed([conversation("only")], storage),
    "saved expanded preference should keep the drawer open",
  );
});

run("a saved collapsed preference overrides the heuristic", () => {
  const storage = fakeStorage();
  saveDrawerCollapsedPreference(true, storage);
  assert(
    initialDrawerCollapsed([conversation("first"), conversation("second")], storage),
    "saved collapsed preference should keep the drawer collapsed",
  );
});

run("garbage stored values fall back to the heuristic", () => {
  const storage = fakeStorage({ "shelley-drawer-collapsed": "maybe" });
  assert(
    !initialDrawerCollapsed([conversation("first"), conversation("second")], storage),
    "invalid stored value should fall back to the heuristic",
  );
});

run("storage errors fall back to the heuristic", () => {
  const storage = fakeStorage();
  storage.getItem = () => {
    throw new Error("denied");
  };
  assert(
    initialDrawerCollapsed([conversation("only")], storage),
    "throwing storage should fall back to the heuristic",
  );
});

console.log("\ndrawerStartup tests passed");
