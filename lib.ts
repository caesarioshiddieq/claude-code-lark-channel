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
  const val = Number(watermark);
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
    const ct = Number(createTime);
    if (Number.isNaN(ct)) continue;
    // Skip already-seen messages (compare numerically to handle varying digit lengths)
    if (last && ct <= Number(last)) continue;

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
      const t = Number(it.create_time);
      return !Number.isNaN(t) && t > Number(max) ? it.create_time : max;
    }, last ?? "0");
  }

  // Sort oldest first by create_time (don't assume API returns any particular order)
  messages.sort((a, b) => Number(a.create_time) - Number(b.create_time));
  return { messages, newWatermark };
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
