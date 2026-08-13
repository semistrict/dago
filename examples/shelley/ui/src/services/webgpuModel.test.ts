import {
  fromQwenText,
  toQwenMessages,
  toQwenTools,
  webGPUErrorMessage,
} from "@semistrict/browser/webgpu-qwen";

function assert(condition: boolean, message: string): void {
  if (!condition) throw new Error(`Assertion failed: ${message}`);
}

const messages = toQwenMessages({
  system_message: { role: "system", content: [{ type: "text", text: "Be useful." }] },
  messages: [
    { role: "human", content: [{ type: "text", text: "Hello" }] },
    { role: "assistant", content: [{ type: "text", text: "Hi" }] },
  ],
});
assert(messages.length === 3, "system message is prepended exactly once");
assert(messages[0].role === "system" && messages[0].content === "Be useful.", "system text");
assert(messages[1].role === "user" && messages[1].content === "Hello", "human role mapping");

const tools = toQwenTools([
  {
    name: "read_file",
    description: "Read a file",
    input_schema: { type: "object", properties: { file_path: { type: "string" } } },
  },
]);
assert(tools?.[0].function.name === "read_file", "tool name mapping");
assert(tools?.[0].function.parameters.type === "object", "tool schema mapping");

const response = fromQwenText("<think>private notes</think>Done");
assert(response.message.role === "assistant", "assistant role mapping");
assert(response.message.content?.[0].text === "Done", "thinking is removed from assistant text");

const toolResponse = fromQwenText(`<tool_call>
<function=read_file>
<parameter=file_path>
/workspace/a
</parameter>
<parameter=line_end>
20
</parameter>
</function>
</tool_call>`);
assert(toolResponse.message.tool_calls?.[0].name === "read_file", "tool name parsing");
assert(
  (toolResponse.message.tool_calls?.[0].arguments as { file_path?: string }).file_path ===
    "/workspace/a",
  "string tool argument parsing",
);
assert(
  (toolResponse.message.tool_calls?.[0].arguments as { line_end?: number }).line_end === 20,
  "JSON scalar tool argument parsing",
);
assert(
  webGPUErrorMessage(new Error("adapter failed")) === "adapter failed",
  "Error messages survive",
);
assert(
  webGPUErrorMessage({ code: "GPUValidationError" }).includes("GPUValidationError"),
  "structured browser errors survive",
);

console.log("webgpuModel tests passed");
