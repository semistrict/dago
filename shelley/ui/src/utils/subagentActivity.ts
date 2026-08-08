// subagentActivity.ts — derive a one-line "what is the subagent doing right
// now" string for the SubagentTool widget. The parent conversation's UI
// already receives every subagent's stream events over /api/stream2 (they're
// routed into messageStore by conversation_id), so this is a pure projection
// of state we already have:
//
//   1. live streaming text (the subagent's in-progress assistant prose)
//   2. live tool progress (tail of a running tool's output)
//   3. the last persisted message: a running tool call (emoji + headline)
//      or the trailing agent text
//   4. the conversation-list preview (survives refresh; no messages needed)
import type { Message, ToolProgress, LLMContent } from "../types";
import { toolEmoji, toolHeadline } from "./toolMeta";

const MAX_LEN = 120;

/** Last non-empty line of a blob of text, truncated to MAX_LEN. */
export function activityTail(text: string): string {
  const lines = text.split(/\r?\n/);
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = lines[i].trim();
    if (!line) continue;
    return line.length > MAX_LEN ? line.slice(0, MAX_LEN) + "\u2026" : line;
  }
  return "";
}

export interface SubagentActivityInput {
  /** In-flight assistant text (messageStore transient streamingText). */
  streamingText?: string;
  /** Running-tool partial output (messageStore transient toolProgress). */
  toolProgress?: Record<string, ToolProgress>;
  /** The subagent conversation's cached messages (may be partial). */
  messages?: Message[];
  /** Conversation-list preview: trailing agent message, always available. */
  preview?: string;
}

function parseContent(msg: Message): LLMContent[] | null {
  if (!msg.llm_data) return null;
  try {
    const data =
      typeof msg.llm_data === "string"
        ? (JSON.parse(msg.llm_data) as { Content?: LLMContent[] })
        : (msg.llm_data as { Content?: LLMContent[] });
    return Array.isArray(data?.Content) ? data.Content : null;
  } catch {
    return null;
  }
}

// Scan messages newest-first for something displayable: a tool_use
// (emoji + headline) or trailing agent text.
function lastMessageActivity(messages: Message[]): string {
  for (let i = messages.length - 1; i >= 0; i--) {
    const msg = messages[i];
    if (msg.type !== "agent" && msg.type !== "tool") continue;
    const content = parseContent(msg);
    if (!content) continue;
    for (let j = content.length - 1; j >= 0; j--) {
      const c = content[j];
      if (c.Type === 5 && c.ToolName) {
        const headline = toolHeadline(c.ToolName, c.ToolInput);
        return `${toolEmoji(c.ToolName, c.ToolInput)} ${headline}`;
      }
      if (c.Type === 2 && c.Text?.trim()) {
        return activityTail(c.Text);
      }
    }
  }
  return "";
}

/** One-line description of what a subagent is doing right now. */
export function subagentActivity(input: SubagentActivityInput): string {
  if (input.streamingText) {
    const tail = activityTail(input.streamingText);
    if (tail) return tail;
  }
  if (input.toolProgress) {
    // Multiple tools can report progress. Object insertion order gives us
    // the most recently STARTED tool (setToolProgress preserves an existing
    // key's position on update), which is the best recency signal available
    // without per-update timestamps.
    const entries = Object.values(input.toolProgress);
    for (let i = entries.length - 1; i >= 0; i--) {
      const p = entries[i];
      const tail = activityTail(p.output || "");
      if (tail) return `${toolEmoji(p.tool_name)} ${tail}`;
    }
  }
  if (input.messages && input.messages.length > 0) {
    const fromMessages = lastMessageActivity(input.messages);
    if (fromMessages) return fromMessages;
  }
  if (input.preview) {
    return activityTail(input.preview);
  }
  return "";
}
