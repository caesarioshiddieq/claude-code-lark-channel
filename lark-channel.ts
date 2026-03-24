#!/usr/bin/env bun
/**
 * Lark Channel for Claude Code
 *
 * Bridges Lark/Feishu messaging into Claude Code sessions.
 * - Polls configured Lark chats for new messages
 * - Forwards them as channel events
 * - Exposes a reply tool so Claude can respond in Lark
 * - Supports permission relay
 */
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import {
  ListToolsRequestSchema,
  CallToolRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import {
  type LarkChat,
  type LarkMessage,
  watermarkToStartTime,
  processItems,
  parsePermissionVerdict,
  buildRichTextContent,
  formatPermissionPrompt,
} from "./lib";

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const APP_ID = process.env.LARK_APP_ID;
const APP_SECRET = process.env.LARK_APP_SECRET;
if (!APP_ID || !APP_SECRET) {
  throw new Error("[lark-channel] LARK_APP_ID and LARK_APP_SECRET env vars are required");
}
const BASE_URL =
  process.env.LARK_BASE_URL ?? "https://open.larksuite.com/open-apis";

// Chats to monitor — comma-separated chat_ids via env, defaults to DM chat
const MONITOR_CHATS = (
  process.env.LARK_CHAT_IDS ?? "oc_4641d16a6e5c12946e4571ffab89365d"
)
  .split(",")
  .map((s) => s.trim())
  .filter(Boolean);

// Allowed sender open_ids — empty means allow all senders in monitored chats
const ALLOWED_SENDERS = process.env.LARK_ALLOWED_SENDERS
  ? process.env.LARK_ALLOWED_SENDERS.split(",").map((s) => s.trim())
  : [];

const POLL_INTERVAL_MS = Math.max(1000, Number(process.env.LARK_POLL_INTERVAL ?? "5000") || 5000);

// ---------------------------------------------------------------------------
// Lark API client
// ---------------------------------------------------------------------------

let appAccessToken = "";
let tokenExpiresAt = 0;
let refreshPromise: Promise<string> | null = null;

async function doRefreshToken(): Promise<string> {
  const res = await fetch(
    `${BASE_URL}/auth/v3/app_access_token/internal`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json; charset=utf-8" },
      body: JSON.stringify({ app_id: APP_ID, app_secret: APP_SECRET }),
    }
  );
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new Error(`[lark-channel] auth HTTP ${res.status}: ${body.slice(0, 500)}`);
  }

  const data = (await res.json()) as {
    code: number;
    msg: string;
    app_access_token: string;
    expire: number;
  };

  if (data.code !== 0) {
    throw new Error(`[lark-channel] auth: ${data.msg}`);
  }

  appAccessToken = data.app_access_token;
  tokenExpiresAt = Date.now() + data.expire * 1000;
  return appAccessToken;
}

async function getAccessToken(): Promise<string> {
  if (appAccessToken && Date.now() < tokenExpiresAt - 300_000) {
    return appAccessToken;
  }
  // Prevent concurrent refresh requests
  if (!refreshPromise) {
    refreshPromise = doRefreshToken().finally(() => { refreshPromise = null; });
  }
  return refreshPromise;
}

async function larkFetch(url: string, init: RequestInit): Promise<unknown> {
  const res = await fetch(url, init);
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new Error(`[lark-channel] HTTP ${res.status}: ${body.slice(0, 500)}`);
  }
  return res.json();
}

async function larkGet(path: string, params?: Record<string, string>) {
  const token = await getAccessToken();
  const url = new URL(`${BASE_URL}${path}`);
  if (params) {
    for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
  }
  return larkFetch(url.toString(), {
    headers: { Authorization: `Bearer ${token}` },
  });
}

async function larkPost(path: string, body: unknown, params?: Record<string, string>) {
  const token = await getAccessToken();
  const url = new URL(`${BASE_URL}${path}`);
  if (params) {
    for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
  }
  return larkFetch(url.toString(), {
    method: "POST",
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json; charset=utf-8",
    },
    body: JSON.stringify(body),
  });
}

// ---------------------------------------------------------------------------
// Chat discovery
// ---------------------------------------------------------------------------

function getChats(): LarkChat[] {
  return MONITOR_CHATS.map((id) => ({ chat_id: id, name: id }));
}

// ---------------------------------------------------------------------------
// Message polling
// ---------------------------------------------------------------------------

const lastSeen = new Map<string, string>();

async function pollChat(chat: LarkChat): Promise<LarkMessage[]> {
  const last = lastSeen.get(chat.chat_id);
  const baseParams: Record<string, string> = {
    container_id_type: "chat",
    container_id: chat.chat_id,
    sort_type: "ByCreateTimeDesc",
    page_size: "50",
  };
  if (last) {
    baseParams.start_time = watermarkToStartTime(last);
  }

  // Paginate to avoid dropping messages in busy chats (max 10 pages)
  const allItems: any[] = [];
  let pageToken: string | undefined;
  let pages = 0;
  do {
    const params = { ...baseParams };
    if (pageToken) params.page_token = pageToken;
    const data = (await larkGet("/im/v1/messages", params)) as any;
    if (data.code !== 0) {
      console.error(`[lark-channel] poll ${chat.chat_id}: ${data.msg}`);
      return [];
    }
    allItems.push(...(data.data?.items ?? []));
    pageToken = data.data?.has_more ? data.data?.page_token : undefined;
    pages++;
  } while (pageToken && pages < 10);

  const result = processItems({
    chat,
    items: allItems,
    lastSeen: last,
    allowedSenders: ALLOWED_SENDERS,
  });

  if (result.newWatermark) {
    lastSeen.set(chat.chat_id, result.newWatermark);
  }

  return result.messages;
}

// ---------------------------------------------------------------------------
// MCP Channel Server
// ---------------------------------------------------------------------------

const mcp = new Server(
  { name: "lark", version: "0.1.0" },
  {
    capabilities: {
      experimental: {
        "claude/channel": {},
        "claude/channel/permission": {},
      },
      tools: {},
    },
    instructions: [
      'Messages from Lark arrive as <channel source="lark" chat_id="..." chat_name="..." sender_id="..." msg_type="...">.',
      "Reply with the lark_reply tool, passing the chat_id from the tag.",
      "Use lark_list_chats to discover available chats.",
      "Messages are from real team members — treat them as high priority.",
    ].join(" "),
  }
);

// --- Tools ------------------------------------------------------------------

mcp.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: "lark_reply",
      description:
        "Send a text message to a Lark chat. Use the chat_id from the inbound channel tag.",
      inputSchema: {
        type: "object" as const,
        properties: {
          chat_id: {
            type: "string",
            description: "The Lark chat_id to reply in",
          },
          text: {
            type: "string",
            description: "The message text to send",
          },
        },
        required: ["chat_id", "text"],
      },
    },
    {
      name: "lark_reply_rich",
      description:
        "Send a rich text (post) message to a Lark chat with formatting support.",
      inputSchema: {
        type: "object" as const,
        properties: {
          chat_id: {
            type: "string",
            description: "The Lark chat_id to reply in",
          },
          title: {
            type: "string",
            description: "Title of the rich text message",
          },
          content: {
            type: "string",
            description:
              "Plain text content. Newlines create separate paragraphs. No formatting markup supported.",
          },
        },
        required: ["chat_id", "content"],
      },
    },
    {
      name: "lark_list_chats",
      description: "List the Lark chats this channel is currently monitoring.",
      inputSchema: {
        type: "object" as const,
        properties: {},
      },
    },
  ],
}));

mcp.setRequestHandler(CallToolRequestSchema, async (req) => {
  const { name, arguments: args } = req.params;

  if (name === "lark_reply") {
    const { chat_id, text } = z.object({ chat_id: z.string(), text: z.string() }).parse(args);
    const data = (await larkPost(
      "/im/v1/messages",
      {
        receive_id: chat_id,
        msg_type: "text",
        content: JSON.stringify({ text }),
      },
      { receive_id_type: "chat_id" }
    )) as any;

    if (data.code !== 0) {
      return {
        content: [{ type: "text" as const, text: `lark_reply failed: ${data.msg}` }],
        isError: true,
      };
    }
    const sentId = data.data?.message_id ?? "";
    return { content: [{ type: "text" as const, text: `Sent to ${chat_id} (${sentId})` }] };
  }

  if (name === "lark_reply_rich") {
    const { chat_id, title, content } = z.object({
      chat_id: z.string(), content: z.string(), title: z.string().optional(),
    }).parse(args);

    const postContent = buildRichTextContent(content, title);

    const data = (await larkPost(
      "/im/v1/messages",
      {
        receive_id: chat_id,
        msg_type: "post",
        content: JSON.stringify(postContent),
      },
      { receive_id_type: "chat_id" }
    )) as any;

    if (data.code !== 0) {
      return {
        content: [{ type: "text" as const, text: `lark_reply_rich failed: ${data.msg}` }],
        isError: true,
      };
    }
    const sentId = data.data?.message_id ?? "";
    return {
      content: [{ type: "text" as const, text: `Sent to ${chat_id} (${sentId})` }],
    };
  }

  if (name === "lark_list_chats") {
    const chats = getChats();
    const list = chats
      .map((c) => `- ${c.name} (${c.chat_id})`)
      .join("\n");
    return {
      content: [
        {
          type: "text" as const,
          text: chats.length > 0 ? list : "No chats found. Check bot permissions.",
        },
      ],
    };
  }

  throw new Error(`[lark-channel] unknown tool: ${name}`);
});

// --- Permission relay -------------------------------------------------------

const PermissionRequestSchema = z.object({
  method: z.literal("notifications/claude/channel/permission_request"),
  params: z.object({
    request_id: z.string(),
    tool_name: z.string(),
    description: z.string(),
    input_preview: z.string(),
  }),
});

mcp.setNotificationHandler(PermissionRequestSchema, async ({ params }) => {
  const chats = MONITOR_CHATS;
  const prompt = formatPermissionPrompt(params);

  for (const chatId of chats) {
    try {
      await larkPost(
        "/im/v1/messages",
        {
          receive_id: chatId,
          msg_type: "text",
          content: JSON.stringify({ text: prompt }),
        },
        { receive_id_type: "chat_id" }
      );
    } catch (e) {
      console.error(`[lark-channel] failed to send permission prompt to ${chatId}:`, e);
    }
  }
});

// --- Connect and start polling ----------------------------------------------

await mcp.connect(new StdioServerTransport());

// Initialize: set high watermark to now so we don't replay old messages
async function startPolling() {
  // Wait a moment for connection to stabilize
  await new Promise((r) => setTimeout(r, 2000));

  const chats = getChats();

  // Set initial watermark to now (captured after stabilization delay)
  const initTime = Date.now().toString();
  for (const chat of chats) {
    lastSeen.set(chat.chat_id, initTime);
  }

  console.error(`Lark channel polling ${chats.length} chat(s): ${chats.map(c => c.chat_id).join(", ")}`);

  // Poll loop
  while (true) {
    try {
      const results = await Promise.allSettled(chats.map(chat => pollChat(chat)));
      for (const [i, result] of results.entries()) {
        if (result.status === "rejected") {
          console.error(`[lark-channel] poll ${chats[i].chat_id} error:`, result.reason);
          continue;
        }
        for (const msg of result.value) {
          // Check for permission verdict
          const verdict = parsePermissionVerdict(msg.body);
          if (verdict) {
            await mcp.notification({
              method: "notifications/claude/channel/permission" as any,
              params: verdict,
            });
            continue;
          }

          // Forward as channel event
          await mcp.notification({
            method: "notifications/claude/channel" as any,
            params: {
              content: msg.body,
              meta: {
                chat_id: msg.chat_id,
                chat_name: msg.chat_name,
                sender_id: msg.sender_id,
                msg_type: msg.msg_type,
                message_id: msg.message_id,
              },
            },
          });
        }
      }
    } catch (e) {
      console.error("[lark-channel] poll error:", e);
    }

    await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
  }
}

startPolling().catch((e) => {
  console.error("[lark-channel] fatal:", e);
  process.exit(1);
});
