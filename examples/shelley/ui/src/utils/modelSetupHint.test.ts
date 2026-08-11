import { canSendWithModel, modelSetupHintKeys, needsModel } from "./modelSetupHint";

function check(name: string, condition: boolean): void {
  if (!condition) throw new Error(name);
}

check("empty catalog uses local setup copy", modelSetupHintKeys().actions.length === 0);
check("empty model cannot send", !canSendWithModel("", ["predictable"]));
check("ready model can send", canSendWithModel("predictable", ["predictable"]));
check("unknown model cannot send", !canSendWithModel("missing", ["predictable"]));
check("shell commands do not need a model", !needsModel("!pwd"));
check("chat needs a model", needsModel("hello"));
