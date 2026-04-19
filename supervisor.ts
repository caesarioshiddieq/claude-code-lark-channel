#!/usr/bin/env bun
/**
 * Lark Supervisor for Claude Code
 *
 * Manages Claude Code session lifecycle from a Lark group chat.
 * - Polls Lark for /start, /stop, /restart commands
 * - Spawns/kills Claude Code in screen sessions
 * - Replies "not active" (rate-limited) when Claude is stopped
 * - Runs as a systemd user service
 */
import { execSync } from "child_process";
import { readdirSync, statSync, readFileSync } from "fs";
import { createLarkClient } from "./lark-client";
import {
  type LarkChat,
  type LarkMessage,
  watermarkToStartTime,
  processItems,
  parseSlashCommand,
  routeSupervisorCommand,
  validateRepoName,
  formatSessionStarted,
  formatSessionStopped,
  formatSessionStatus,
  formatNotActive,
  formatUnauthorized,
  formatOperationInProgress,
  formatInvalidRepo,
  type SessionInfo,
} from "./lib";

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const APP_ID = process.env.LARK_APP_ID;
const APP_SECRET = process.env.LARK_APP_SECRET;
if (!APP_ID || !APP_SECRET) {
  throw new Error("[supervisor] LARK_APP_ID and LARK_APP_SECRET env vars are required");
}

const BASE_URL = process.env.LARK_BASE_URL ?? "https://open.larksuite.com/open-apis";

const rawChatIds = process.env.LARK_CHAT_IDS;
if (!rawChatIds) {
  throw new Error("[supervisor] LARK_CHAT_IDS env var is required");
}
const MONITOR_CHATS = rawChatIds.split(",").map((s) => s.trim()).filter(Boolean);

const ALLOWED_SENDERS = process.env.LARK_ALLOWED_SENDERS
  ? process.env.LARK_ALLOWED_SENDERS.split(",").map((s) => s.trim()).filter(Boolean)
  : [];

const POLL_INTERVAL_MS = Math.max(1000, +(process.env.LARK_POLL_INTERVAL ?? "5000") || 5000);

const REPOS_DIR = process.env.REPOS_DIR ?? "/home/sessions/caesario/repos";
const DEFAULT_REPO = process.env.DEFAULT_REPO ?? "internal-affairs";
const SCREEN_SESSION = "claude";
const ENV_FILE = `${process.env.HOME}/.config/systemd/user/claude-code.env`;

// ---------------------------------------------------------------------------
// Lark client
// ---------------------------------------------------------------------------

const lark = createLarkClient({ appId: APP_ID, appSecret: APP_SECRET, baseUrl: BASE_URL });

// ---------------------------------------------------------------------------
// Session state
// ---------------------------------------------------------------------------

let currentRepo: string | null = null;
let sessionStartedAt: number | null = null;
let lifecycleLock = false;

// Rate limiter for "not active" replies: senderId -> lastReplyTimestamp
const notActiveReplies = new Map<string, number>();
const NOT_ACTIVE_COOLDOWN_MS = 60_000;

function isSessionRunning(): boolean {
  try {
    const output = execSync("screen -ls", { encoding: "utf8" });
    return output.includes(`.${SCREEN_SESSION}\t`);
  } catch (e: unknown) {
    // screen -ls returns exit code 1 when detached sessions exist
    if (e && typeof e === "object" && "stdout" in e) {
      const stdout = (e as { stdout: string }).stdout;
      return stdout.includes(`.${SCREEN_SESSION}\t`);
    }
    return false;
  }
}

function getAvailableRepos(): string[] {
  try {
    return readdirSync(REPOS_DIR).filter((name) => {
      try {
        return statSync(`${REPOS_DIR}/${name}/.git`).isDirectory();
      } catch {
        return false;
      }
    });
  } catch {
    return [];
  }
}

// ---------------------------------------------------------------------------
// Lifecycle operations (mutex-protected)
// ---------------------------------------------------------------------------

async function withLifecycleLock<T>(fn: () => Promise<T>): Promise<T | null> {
  if (lifecycleLock) return null;
  lifecycleLock = true;
  try {
    return await fn();
  } finally {
    lifecycleLock = false;
  }
}

async function startSession(repo: string): Promise<string> {
  const repoPath = `${REPOS_DIR}/${repo}`;
  // Safe: repoPath is allowlist-validated, no user input in shell
  execSync(
    `screen -dmS ${SCREEN_SESSION} bash -c 'source ${ENV_FILE} && cd "${repoPath}" && claude --dangerously-skip-permissions --dangerously-load-development-channels server:lark'`,
  );

  // Wait for development channel confirmation prompt and auto-confirm
  const confirmed = await waitForPromptAndConfirm(SCREEN_SESSION, 15_000);
  if (!confirmed) {
    console.error("[supervisor] development channel prompt not detected within 15s");
  }

  currentRepo = repo;
  sessionStartedAt = Date.now();
  return formatSessionStarted(repo);
}

async function waitForPromptAndConfirm(session: string, timeoutMs: number): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      execSync(`screen -S ${session} -X hardcopy /tmp/screen-check.txt`);
      const content = readFileSync("/tmp/screen-check.txt", "utf8");
      if (content.includes("Enter to confirm")) {
        execSync(`screen -S ${session} -X stuff "$(printf '\\r')"`);
        return true;
      }
    } catch {
      // screen session may not be ready yet
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  return false;
}

function stopSession(): string {
  try {
    execSync(`screen -S ${SCREEN_SESSION} -X quit`);
  } catch {
    // session may already be dead
  }
  currentRepo = null;
  sessionStartedAt = null;
  return formatSessionStopped();
}

// ---------------------------------------------------------------------------
// Command handlers
// ---------------------------------------------------------------------------

async function handleLifecycleCommand(
  command: string,
  args: string,
  chatId: string,
  senderId: string,
): Promise<void> {
  // Authorization check
  if (ALLOWED_SENDERS.length > 0 && !ALLOWED_SENDERS.includes(senderId)) {
    await replyToChat(chatId, formatUnauthorized());
    return;
  }

  // Mutex check
  const result = await withLifecycleLock(async () => {
    // CODEX FIX #5: wrap switch in try-catch
    try {
      switch (command) {
        case "start": {
          if (isSessionRunning()) {
            return `Session is already running in ${currentRepo ?? "unknown"}. Send /restart to restart.`;
          }
          const repoName = args || DEFAULT_REPO;
          const available = getAvailableRepos();
          const validated = validateRepoName(repoName, available);
          if (!validated) {
            return formatInvalidRepo(repoName, available);
          }
          return await startSession(validated);
        }
        case "stop": {
          if (!isSessionRunning()) {
            return "Session is not running.";
          }
          return stopSession();
        }
        case "restart": {
          // CODEX FIX #4: save previousRepo BEFORE calling stopSession()
          const previousRepo = currentRepo;
          if (isSessionRunning()) {
            stopSession();
            await new Promise((r) => setTimeout(r, 2000));
          }
          const repoName = args || previousRepo || DEFAULT_REPO;
          const available = getAvailableRepos();
          const validated = validateRepoName(repoName, available);
          if (!validated) {
            return formatInvalidRepo(repoName, available);
          }
          return await startSession(validated);
        }
        default:
          return "Unknown lifecycle command.";
      }
    } catch (err) {
      // CODEX FIX #5: reply with error on failure
      const message = err instanceof Error ? err.message : String(err);
      return `Failed to ${command} session: ${message}`;
    }
  });

  if (result === null) {
    await replyToChat(chatId, formatOperationInProgress());
  } else {
    await replyToChat(chatId, result);
  }
}

function handleQueryCommand(command: string): string {
  switch (command) {
    case "status": {
      const running = isSessionRunning();
      const uptimeSeconds = running && sessionStartedAt
        ? (Date.now() - sessionStartedAt) / 1000
        : 0;
      const info: SessionInfo = {
        running,
        repo: currentRepo,
        uptimeSeconds,
        monitoredChats: MONITOR_CHATS,
      };
      return formatSessionStatus(info);
    }
    case "help-channel": {
      const running = isSessionRunning();
      const lines = [
        `Lark Supervisor Commands`,
        ``,
        `Lifecycle (authorized users only):`,
        `  /start [repo] — Start Claude Code session (default: ${DEFAULT_REPO})`,
        `  /stop — Stop the active session`,
        `  /restart [repo] — Restart the session`,
        ``,
        `Query:`,
        `  /status — Show session status`,
        `  /help-channel — This help message`,
      ];
      if (running) {
        lines.push(
          ``,
          `Claude Code commands (forwarded when session is active):`,
          `  /clear, /compact, /model, /help, /config, /cost, ...`,
        );
      }
      return lines.join("\n");
    }
    default:
      return "Unknown query command.";
  }
}

// ---------------------------------------------------------------------------
// Lark reply helper
// ---------------------------------------------------------------------------

async function replyToChat(chatId: string, text: string): Promise<void> {
  try {
    // CODEX FIX #7: check Lark API response code
    const data = (await lark.post(
      "/im/v1/messages",
      {
        receive_id: chatId,
        msg_type: "text",
        content: JSON.stringify({ text }),
      },
      { receive_id_type: "chat_id" },
    )) as { code: number; msg?: string };
    if (data.code !== 0) {
      console.error(`[supervisor] reply to ${chatId} API error: ${data.msg}`);
    }
  } catch (e) {
    console.error(`[supervisor] reply to ${chatId} failed:`, e);
  }
}

// ---------------------------------------------------------------------------
// Rate-limited "not active" reply
// ---------------------------------------------------------------------------

function shouldSendNotActive(senderId: string): boolean {
  const lastReply = notActiveReplies.get(senderId);
  if (lastReply && Date.now() - lastReply < NOT_ACTIVE_COOLDOWN_MS) {
    return false;
  }
  notActiveReplies.set(senderId, Date.now());
  return true;
}

function pruneNotActiveMap(): void {
  const cutoff = Date.now() - 5 * 60_000;
  for (const [key, ts] of notActiveReplies) {
    if (ts < cutoff) notActiveReplies.delete(key);
  }
}

// ---------------------------------------------------------------------------
// Message polling
// ---------------------------------------------------------------------------

function getChats(): LarkChat[] {
  return MONITOR_CHATS.map((id) => ({ chat_id: id, name: id }));
}

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

  const allItems: any[] = [];
  let pageToken: string | undefined;
  let pages = 0;
  do {
    const params = { ...baseParams };
    if (pageToken) params.page_token = pageToken;
    const data = (await lark.get("/im/v1/messages", params)) as any;
    if (data.code !== 0) {
      console.error(`[supervisor] poll ${chat.chat_id}: ${data.msg}`);
      return [];
    }
    allItems.push(...(data.data?.items ?? []));
    pageToken = data.data?.has_more ? data.data?.page_token : undefined;
    pages++;
  } while (pageToken && pages < 10);

  // Supervisor does NOT filter by ALLOWED_SENDERS for polling — auth is per-command
  const result = processItems({
    chat,
    items: allItems,
    lastSeen: last,
    allowedSenders: [],
  });

  if (result.newWatermark) {
    lastSeen.set(chat.chat_id, result.newWatermark);
  }

  return result.messages;
}

// ---------------------------------------------------------------------------
// Main poll loop
// ---------------------------------------------------------------------------

async function startPolling() {
  const chats = getChats();

  // Set initial watermark to now
  const initTime = Date.now().toString();
  for (const chat of chats) {
    lastSeen.set(chat.chat_id, initTime);
  }

  // CODEX FIX #8: Detect existing session with warning about unknown state
  if (isSessionRunning()) {
    currentRepo = currentRepo ?? "unknown";
    sessionStartedAt = sessionStartedAt ?? Date.now();
    console.error(`[supervisor] detected existing screen session (repo: ${currentRepo}, uptime: unknown — supervisor was restarted)`);
  }

  console.error(`[supervisor] polling ${chats.length} chat(s): ${chats.map((c) => c.chat_id).join(", ")}`);

  while (true) {
    try {
      pruneNotActiveMap();

      const results = await Promise.allSettled(chats.map((chat) => pollChat(chat)));
      for (const [i, result] of results.entries()) {
        if (result.status === "rejected") {
          console.error(`[supervisor] poll ${chats[i].chat_id} error:`, result.reason);
          continue;
        }

        // CODEX FIX #9: check isSessionRunning() per message, not per batch
        for (const msg of result.value) {
          // Skip bot's own messages (already filtered by processItems, but be safe)
          if (msg.sender_type === "app") continue;

          const running = isSessionRunning();  // per-message check
          const command = parseSlashCommand(msg.body);

          if (command) {
            const route = routeSupervisorCommand(command);

            switch (route.kind) {
              case "lifecycle":
                await handleLifecycleCommand(route.command, command.args, msg.chat_id, msg.sender_id);
                break;
              case "query":
                await replyToChat(msg.chat_id, handleQueryCommand(route.command));
                break;
              case "passthrough":
                // Ignore — lark-channel MCP handles these when Claude is running
                if (!running) {
                  if (shouldSendNotActive(msg.sender_id)) {
                    await replyToChat(msg.chat_id, formatNotActive());
                  }
                }
                break;
              case "unknown":
                await replyToChat(msg.chat_id, route.text);
                break;
            }
          } else {
            // Regular (non-command) message
            if (!running) {
              if (shouldSendNotActive(msg.sender_id)) {
                await replyToChat(msg.chat_id, formatNotActive());
              }
            }
            // When running, lark-channel MCP handles regular messages
          }
        }
      }
    } catch (e) {
      console.error("[supervisor] poll error:", e);
    }

    await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
  }
}

startPolling().catch((e) => {
  console.error("[supervisor] fatal:", e);
  process.exit(1);
});
