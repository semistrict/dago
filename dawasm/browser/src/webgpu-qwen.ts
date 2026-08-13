// Qwen WebGPU inference support for browser-hosted Go agents.
import {
  AutoProcessor,
  InterruptableStoppingCriteria,
  Qwen3_5ForCausalLM,
  type ProgressInfo,
  type Tensor,
} from "@huggingface/transformers";

export const webGPUModelID = "onnx-community/Qwen3.5-0.8B-ONNX-OPT";
export const webGPUModelLabel = "Qwen3.5 0.8B (WebGPU)";

export type WebGPUProgressReport = {
  progress: number;
  text: string;
};

type dagoContentBlock = {
  type: string;
  text?: string;
};

type dagoToolCall = {
  id: string;
  name: string;
  arguments: unknown;
};

type dagoMessage = {
  role: "human" | "assistant" | "system" | "tool" | "remove";
  content?: dagoContentBlock[];
  tool_calls?: dagoToolCall[];
  tool_call_id?: string;
};

type dagoTool = {
  name: string;
  description: string;
  input_schema: Record<string, unknown>;
};

type dagoModelRequest = {
  messages: dagoMessage[];
  system_message?: dagoMessage;
  tools?: dagoTool[];
  stop?: string[];
};

type dagoModelResponse = {
  message: {
    role: "assistant";
    content?: Array<{ type: "text"; text: string }>;
    tool_calls?: dagoToolCall[];
  };
};

type QwenMessage = {
  role: "system" | "user" | "assistant" | "tool";
  content: string;
  tool_calls?: Array<{
    id: string;
    type: "function";
    function: { name: string; arguments: unknown };
  }>;
};

type QwenTool = {
  type: "function";
  function: {
    name: string;
    description: string;
    parameters: Record<string, unknown>;
  };
};

function messageText(message: dagoMessage): string {
  return (message.content || [])
    .filter((block) => block.type === "text")
    .map((block) => block.text || "")
    .join("");
}

export function toQwenMessages(request: dagoModelRequest): QwenMessage[] {
  const source = request.system_message
    ? [request.system_message, ...(request.messages || [])]
    : request.messages || [];
  const result: QwenMessage[] = [];
  for (const message of source) {
    switch (message.role) {
      case "system":
        result.push({ role: "system", content: messageText(message) });
        break;
      case "human":
        result.push({ role: "user", content: messageText(message) });
        break;
      case "assistant":
        result.push({
          role: "assistant",
          content: messageText(message),
          ...(message.tool_calls?.length
            ? {
                tool_calls: message.tool_calls.map((call) => ({
                  id: call.id,
                  type: "function" as const,
                  function: { name: call.name, arguments: call.arguments },
                })),
              }
            : {}),
        });
        break;
      case "tool":
        result.push({ role: "tool", content: messageText(message) });
        break;
      case "remove":
        break;
    }
  }
  return result;
}

export function toQwenTools(
  tools: dagoTool[] | undefined,
): QwenTool[] | undefined {
  if (!tools?.length) return undefined;
  return tools.map((tool) => ({
    type: "function",
    function: {
      name: tool.name,
      description: tool.description,
      parameters: tool.input_schema,
    },
  }));
}

function parseParameter(value: string): unknown {
  const normalized = value.trim();
  try {
    return JSON.parse(normalized) as unknown;
  } catch {
    return normalized;
  }
}

function stripThinking(text: string): string {
  return text.replace(/<think>[\s\S]*?<\/think>/g, "").trim();
}

export function fromQwenText(text: string): dagoModelResponse {
  const toolCalls: dagoToolCall[] = [];
  const toolCallPattern =
    /<tool_call>\s*<function=([^>\n]+)>([\s\S]*?)<\/function>\s*<\/tool_call>/g;
  for (const match of text.matchAll(toolCallPattern)) {
    const parameters: Record<string, unknown> = {};
    const parameterPattern =
      /<parameter=([^>\n]+)>\s*([\s\S]*?)\s*<\/parameter>/g;
    for (const parameter of match[2].matchAll(parameterPattern)) {
      parameters[parameter[1].trim()] = parseParameter(parameter[2]);
    }
    toolCalls.push({
      id: crypto.randomUUID(),
      name: match[1].trim(),
      arguments: parameters,
    });
  }

  const content = stripThinking(text.replace(toolCallPattern, ""));
  const response: dagoModelResponse = { message: { role: "assistant" } };
  if (content) response.message.content = [{ type: "text", text: content }];
  if (toolCalls.length) response.message.tool_calls = toolCalls;
  return response;
}

function truncateAtStop(text: string, stops: string[] | undefined): string {
  let end = text.length;
  for (const stop of stops || []) {
    if (!stop) continue;
    const index = text.indexOf(stop);
    if (index >= 0 && index < end) end = index;
  }
  return text.slice(0, end);
}

export function webGPUErrorMessage(error: unknown): string {
  if (error instanceof Error && error.message) return error.message;
  if (typeof error === "string" && error) return error;
  try {
    const encoded = JSON.stringify(error);
    if (encoded && encoded !== "{}") return encoded;
  } catch {
    // Fall through to the stable fallback below.
  }
  return String(error || "unknown browser error");
}

function progressReport(info: ProgressInfo): WebGPUProgressReport | null {
  if (info.status === "progress_total") {
    return {
      progress: Math.min(1, Math.max(0, info.progress / 100)),
      text: `Downloading Qwen3.5… ${Math.round(info.progress)}%`,
    };
  }
  if (info.status === "progress") {
    return {
      progress: Math.min(1, Math.max(0, info.progress / 100)),
      text: `Downloading Qwen3.5… ${Math.round(info.progress)}%`,
    };
  }
  return null;
}

type LoadedModel = Awaited<
  ReturnType<typeof Qwen3_5ForCausalLM.from_pretrained>
>;
type LoadedProcessor = Awaited<
  ReturnType<typeof AutoProcessor.from_pretrained>
>;

let model: LoadedModel | null = null;
let processor: LoadedProcessor | null = null;
let loading: Promise<void> | null = null;
let stoppingCriteria: InterruptableStoppingCriteria | null = null;

export async function loadWebGPUModel(
  onProgress: (report: WebGPUProgressReport) => void,
): Promise<void> {
  if (model && processor) return;
  if (!("gpu" in navigator)) {
    throw new Error(
      "WebGPU is not available in this browser. Use a current Chromium browser or connect OpenAI.",
    );
  }

  loading ??= (async () => {
    onProgress({ progress: 0, text: "Loading Qwen3.5 processor…" });
    processor = await AutoProcessor.from_pretrained(webGPUModelID);
    model = await Qwen3_5ForCausalLM.from_pretrained(webGPUModelID, {
      device: "webgpu",
      dtype: {
        embed_tokens: "q4f16",
        decoder_model_merged: "q4f16",
      },
      progress_callback: (info) => {
        const report = progressReport(info);
        if (report) onProgress(report);
      },
    });
    onProgress({ progress: 1, text: "Qwen3.5 is ready" });
  })();

  try {
    await loading;
  } catch (error) {
    model = null;
    processor = null;
    loading = null;
    throw error;
  }
}

export async function invokeWebGPUModel(encoded: string): Promise<string> {
  if (!model || !processor) throw new Error("WebGPU model is not loaded");
  const activeModel = model;
  const activeProcessor = processor;
  let stage = "request decoding";
  try {
    const request = JSON.parse(encoded) as dagoModelRequest;
    stage = "prompt construction";
    const prompt = activeProcessor.apply_chat_template(
      toQwenMessages(request),
      {
        add_generation_prompt: true,
        tokenize: false,
        tools: toQwenTools(request.tools),
      },
    );
    stage = "tokenization";
    const inputs = await activeProcessor(prompt);
    const criteria = new InterruptableStoppingCriteria();
    stoppingCriteria = criteria;
    let outputs: Tensor;
    try {
      stage = "generation";
      outputs = (await activeModel.generate({
        ...inputs,
        max_new_tokens: 1024,
        do_sample: false,
        stopping_criteria: criteria,
      })) as Tensor;
    } finally {
      if (stoppingCriteria === criteria) stoppingCriteria = null;
    }
    stage = "output decoding";
    const promptLength = inputs.input_ids.dims.at(-1) || 0;
    const completionTokens = outputs.slice(
      [0, outputs.dims[0]],
      [promptLength, outputs.dims[1]],
    );
    const completion = activeProcessor.tokenizer!.batch_decode(
      completionTokens,
      {
        skip_special_tokens: true,
      },
    )[0];
    return JSON.stringify(
      fromQwenText(truncateAtStop(completion, request.stop)),
    );
  } catch (error) {
    console.error(`Qwen3.5 ${stage} failed`, error);
    throw new Error(`Qwen3.5 ${stage} failed: ${webGPUErrorMessage(error)}`);
  }
}

export async function interruptWebGPUModel(): Promise<void> {
  stoppingCriteria?.interrupt();
}
