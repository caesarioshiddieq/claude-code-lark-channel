/**
 * Lark Channel — Pure/testable functions
 *
 * Extracted from lark-channel.ts so they can be unit-tested
 * without spawning an MCP server or hitting the Lark API.
 */

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface LarkChat {
  chat_id: string;
  name: string;
}

export interface LarkMessage {
  message_id: string;
  chat_id: string;
  chat_name: string;
  sender_id: string;
  sender_type: string;
  msg_type: string;
  create_time: string;
  body: string;
}

export interface LarkApiItem {
  message_id: string;
  msg_type: string;
  create_time: string;
  sender?: {
    sender_type?: string;
    sender_id?: { open_id?: string; user_id?: string };
  };
  body?: { content?: string };
}

// ---------------------------------------------------------------------------
// extractTextContent — parse Lark message content into plain text
// ---------------------------------------------------------------------------

export function extractTextContent(msg_type: string, contentStr: string): string {
  try {
    const content = JSON.parse(contentStr);
    switch (msg_type) {
      case "text":
        return content.text ?? contentStr;
      case "post": {
        interface PostElement {
          tag: string;
          text?: string;
          href?: string;
          user_name?: string;
          user_id?: string;
        }
        const lines: string[] = [];
        const title = content.title;
        if (title) lines.push(`**${title}**`);
        for (const [key, lang] of Object.entries(content)) {
          if (key === "title") continue;
          if (!Array.isArray(lang)) continue;
          for (const para of lang) {
            if (!Array.isArray(para)) continue;
            lines.push(
              (para as PostElement[])
                .map((el) => {
                  if (el.tag === "text") return el.text ?? "";
                  if (el.tag === "a") return `[${el.text}](${el.href})`;
                  if (el.tag === "at") return `@${el.user_name ?? el.user_id ?? "unknown"}`;
                  return "";
                })
                .join("")
            );
          }
        }
        return lines.join("\n");
      }
      case "interactive":
        return `[Card message] ${contentStr.slice(0, 500)}`;
      case "image":
        return "[Image]";
      case "file":
        return `[File: ${content.file_name ?? "unknown"}]`;
      default:
        return `[${msg_type}] ${contentStr.slice(0, 300)}`;
    }
  } catch {
    return contentStr;
  }
}

// ---------------------------------------------------------------------------
// watermarkToStartTime — convert stored watermark to Lark API start_time
// ---------------------------------------------------------------------------

export function watermarkToStartTime(watermark: string): string {
  const val = +watermark;
  if (Number.isNaN(val)) return watermark;
  // create_time is in milliseconds (13 digits); Lark API start_time expects seconds
  return val > 9_999_999_999
    ? Math.floor(val / 1000).toString()
    : watermark;
}

// ---------------------------------------------------------------------------
// processItems — filter and transform raw API items into LarkMessages
// ---------------------------------------------------------------------------

export interface ProcessItemsOptions {
  chat: LarkChat;
  items: LarkApiItem[];
  lastSeen: string | undefined;
  allowedSenders: string[];
}

export interface ProcessItemsResult {
  messages: LarkMessage[];
  newWatermark: string | undefined;
}

export function processItems(opts: ProcessItemsOptions): ProcessItemsResult {
  const { chat, items, lastSeen: last, allowedSenders } = opts;
  const messages: LarkMessage[] = [];

  for (const item of items) {
    // Skip bot's own messages
    if (item.sender?.sender_type === "app") continue;

    const createTime = item.create_time;
    const ct = +createTime;
    if (Number.isNaN(ct)) continue;
    // Skip already-seen messages (compare numerically to handle varying digit lengths)
    if (last && ct <= +last) continue;

    const senderId =
      item.sender?.sender_id?.open_id ??
      item.sender?.sender_id?.user_id ??
      "unknown";

    // Sender gating
    if (allowedSenders.length > 0 && !allowedSenders.includes(senderId)) {
      continue;
    }

    messages.push({
      message_id: item.message_id,
      chat_id: chat.chat_id,
      chat_name: chat.name,
      sender_id: senderId,
      sender_type: item.sender?.sender_type ?? "user",
      msg_type: item.msg_type,
      create_time: createTime,
      body: extractTextContent(item.msg_type, item.body?.content ?? ""),
    });
  }

  // Compute new watermark from all items (including bot messages)
  let newWatermark: string | undefined;
  if (items.length > 0) {
    newWatermark = items.reduce((max: string, it) => {
      const t = +it.create_time;
      return !Number.isNaN(t) && t > +max ? it.create_time : max;
    }, last ?? "0");
  }

  // Sort oldest first by create_time (don't assume API returns any particular order)
  const sorted = [...messages].sort((a, b) => +a.create_time - +b.create_time);
  return { messages: sorted, newWatermark };
}

// ---------------------------------------------------------------------------
// Permission verdict parsing
// ---------------------------------------------------------------------------

// [a-km-z] excludes 'l' to avoid l/1 ambiguity on mobile keyboards.
// Claude Code generates 5-char IDs from this alphabet.
export const PERMISSION_REPLY_RE = /^\s*(y|yes|n|no)\s+([a-km-z]{5})\s*$/i;

export function parsePermissionVerdict(
  text: string
): { request_id: string; behavior: "allow" | "deny" } | null {
  const m = PERMISSION_REPLY_RE.exec(text);
  if (!m) return null;
  return {
    request_id: m[2].toLowerCase(),
    behavior: m[1].toLowerCase().startsWith("y") ? "allow" : "deny",
  };
}

// ---------------------------------------------------------------------------
// buildRichTextContent — convert plain text to Lark post format
// ---------------------------------------------------------------------------

export function buildRichTextContent(
  content: string,
  title?: string
): Record<string, unknown> {
  const paragraphs = content.split("\n").map((line) => [
    { tag: "text", text: line },
  ]);
  return {
    en_us: { title: title ?? "", content: paragraphs },
  };
}

// ---------------------------------------------------------------------------
// formatPermissionPrompt — format a permission request for the chat
// ---------------------------------------------------------------------------

export function formatPermissionPrompt(params: {
  request_id: string;
  tool_name: string;
  description: string;
  input_preview: string;
}): string {
  const preview = params.input_preview.length > 150
    ? params.input_preview.slice(0, 150) + "..."
    : params.input_preview;
  return [
    `🔐 Permission Request`,
    `Reply "yes ${params.request_id}" or "no ${params.request_id}"`,
    ``,
    `Tool: ${params.tool_name}`,
    `Description: ${params.description}`,
    `Preview: ${preview}`,
  ].join("\n");
}

// ---------------------------------------------------------------------------
// Slash command types
// ---------------------------------------------------------------------------

export interface SlashCommand {
  readonly name: string;
  readonly args: string;
  readonly raw: string;
}

export type CommandRoute =
  | { readonly kind: "forward" }
  | { readonly kind: "mcp"; readonly handler: string }
  | { readonly kind: "unknown"; readonly text: string };

export interface CommandInfo {
  readonly name: string;
  readonly description: string;
  readonly category: "claude" | "mcp";
}

export interface StatusInfo {
  readonly uptimeSeconds: number;
  readonly monitoredChats: readonly string[];
  readonly pollIntervalMs: number;
  readonly lastPollTimes: Readonly<Record<string, string>>;
}

// ---------------------------------------------------------------------------
// Slash command parsing
// ---------------------------------------------------------------------------

const SLASH_CMD_RE = /^\/([a-z][a-z0-9-]*)(?:\s+(.*))?$/i;

export function parseSlashCommand(text: string): SlashCommand | null {
  const trimmed = text.trim();
  const m = SLASH_CMD_RE.exec(trimmed);
  if (!m) return null;
  return { name: m[1].toLowerCase(), args: (m[2] ?? "").trim(), raw: trimmed };
}

// ---------------------------------------------------------------------------
// Command registry and routing
// ---------------------------------------------------------------------------

const CLAUDE_BUILTINS = new Set([
  "clear", "compact", "config", "cost", "doctor", "help", "login",
  "logout", "memory", "model", "permissions", "review", "terminal-setup", "vim",
]);

const MCP_COMMAND_REGISTRY: readonly CommandInfo[] = [
  { name: "status", description: "Show channel uptime, monitored chats, and poll info", category: "mcp" },
  { name: "help-channel", description: "List all available slash commands", category: "mcp" },
];

const MCP_COMMANDS = new Set(MCP_COMMAND_REGISTRY.map((c) => c.name));

export function getAvailableCommands(): readonly CommandInfo[] {
  const claude: CommandInfo[] = [...CLAUDE_BUILTINS].sort().map((name) => ({
    name,
    description: `Claude Code built-in /${name}`,
    category: "claude" as const,
  }));
  return [...MCP_COMMAND_REGISTRY, ...claude];
}

export function routeCommand(cmd: SlashCommand): CommandRoute {
  if (MCP_COMMANDS.has(cmd.name)) {
    return { kind: "mcp", handler: cmd.name };
  }
  if (CLAUDE_BUILTINS.has(cmd.name)) {
    return { kind: "forward" };
  }
  return {
    kind: "unknown",
    text: formatUnknownCommandReply(cmd.name, getAvailableCommands()),
  };
}

// ---------------------------------------------------------------------------
// Slash command formatters
// ---------------------------------------------------------------------------

export function formatUnknownCommandReply(
  commandName: string,
  available: readonly CommandInfo[],
): string {
  const list = available.map((c) => `  /${c.name} — ${c.description}`).join("\n");
  return `Unknown command: /${commandName}\n\nAvailable commands:\n${list}`;
}

export function formatStatusReply(info: StatusInfo): string {
  const uptime = Math.floor(info.uptimeSeconds);
  const h = Math.floor(uptime / 3600);
  const m = Math.floor((uptime % 3600) / 60);
  const s = uptime % 60;
  const uptimeStr = h > 0 ? `${h}h ${m}m ${s}s` : m > 0 ? `${m}m ${s}s` : `${s}s`;
  const chats = info.monitoredChats.join(", ");
  const polls = Object.entries(info.lastPollTimes)
    .map(([id, ts]) => `  ${id}: last seen ${ts}`)
    .join("\n");
  return [
    `Channel Status`,
    `Uptime: ${uptimeStr}`,
    `Poll interval: ${info.pollIntervalMs}ms`,
    `Monitored chats: ${chats}`,
    polls ? `Last seen:\n${polls}` : "",
  ].filter(Boolean).join("\n");
}

export function formatHelpChannelReply(commands: readonly CommandInfo[]): string {
  const mcpCmds = commands.filter((c) => c.category === "mcp");
  const claudeCmds = commands.filter((c) => c.category === "claude");
  const mcpList = mcpCmds.map((c) => `  /${c.name} — ${c.description}`).join("\n");
  const claudeList = claudeCmds.map((c) => `  /${c.name}`).join("\n");
  return [
    `Lark Channel Commands`,
    ``,
    `Channel commands:`,
    mcpList,
    ``,
    `Claude Code commands (forwarded):`,
    claudeList,
  ].join("\n");
}

// ---------------------------------------------------------------------------
// Supervisor types and routing
// ---------------------------------------------------------------------------

export type SupervisorRoute =
  | { readonly kind: "lifecycle"; readonly command: "start" | "stop" | "restart" }
  | { readonly kind: "query"; readonly command: "status" | "help-channel" }
  | { readonly kind: "passthrough" }
  | { readonly kind: "unknown"; readonly text: string };

export interface SessionInfo {
  readonly running: boolean;
  readonly repo: string | null;
  readonly uptimeSeconds: number;
  readonly monitoredChats: readonly string[];
}

const LIFECYCLE_COMMANDS = new Set(["start", "stop", "restart"]);
const QUERY_COMMANDS = new Set(["status", "help-channel"]);

export function routeSupervisorCommand(cmd: SlashCommand): SupervisorRoute {
  if (LIFECYCLE_COMMANDS.has(cmd.name)) {
    return { kind: "lifecycle", command: cmd.name as "start" | "stop" | "restart" };
  }
  if (QUERY_COMMANDS.has(cmd.name)) {
    return { kind: "query", command: cmd.name as "status" | "help-channel" };
  }
  if (CLAUDE_BUILTINS.has(cmd.name)) {
    return { kind: "passthrough" };
  }
  return { kind: "unknown", text: formatUnknownSupervisorReply(cmd.name) };
}

function formatUnknownSupervisorReply(commandName: string): string {
  return [
    `Unknown command: /${commandName}`,
    ``,
    `Lifecycle commands:`,
    `  /start [repo] — Start a Claude Code session`,
    `  /stop — Stop the active session`,
    `  /restart [repo] — Restart the session`,
    ``,
    `Query commands:`,
    `  /status — Show session status`,
    `  /help-channel — List all commands`,
  ].join("\n");
}
