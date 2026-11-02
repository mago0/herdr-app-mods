// Links a PR to this pi session as soon as a bash call that ran `gh pr create` finishes.
// herdr-pr decides what the command created; this only hands it the command and its output.
import { spawn } from "node:child_process";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Cheap test on the command text, so large outputs of unrelated calls are never copied.
const MAYBE_CREATE = /\bpr\s+create\b/;

function text(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .filter((block) => block?.type === "text" && typeof block.text === "string")
    .map((block) => block.text)
    .join("\n");
}

export default function (pi: ExtensionAPI) {
  pi.on("tool_result", (event: any, ctx: any) => {
    if (!process.env.HC_TERMINAL_ID || event?.toolName !== "bash") return undefined;
    const command = event?.input?.command;
    if (typeof command !== "string" || !MAYBE_CREATE.test(command)) return undefined;
    const output = text(event?.content ?? event?.result?.content);
    const env = { ...process.env };
    const id = ctx?.sessionManager?.getSessionId?.();
    const file = ctx?.sessionManager?.getSessionFile?.();
    if (id) env.PI_SESSION_ID = id;
    if (file) env.PI_SESSION_FILE = file;
    try {
      const child = spawn("herdr-pr", ["hook"], { detached: true, stdio: ["pipe", "ignore", "ignore"], env });
      child.on("error", () => {});
      child.stdin?.on("error", () => {});
      child.stdin?.end(JSON.stringify({ command, output }));
      child.unref();
    } catch {
      // herdr-pr missing or not runnable: linking is best effort and must not touch the tool call.
    }
    return undefined;
  });
}
