import { describe, test, expect } from "bun:test";
import {
  extractTextContent,
  watermarkToStartTime,
  processItems,
  parsePermissionVerdict,
  buildRichTextContent,
  formatPermissionPrompt,
  parseSlashCommand,
  routeCommand,
  getAvailableCommands,
  formatUnknownCommandReply,
  formatStatusReply,
  formatHelpChannelReply,
  routeSupervisorCommand,
  validateRepoName,
  type LarkApiItem,
  type LarkChat,
  type StatusInfo,
  type SupervisorRoute,
} from "./lib";

// ---------------------------------------------------------------------------
// extractTextContent
// ---------------------------------------------------------------------------

describe("extractTextContent", () => {
  test("extracts plain text", () => {
    expect(extractTextContent("text", '{"text":"hello world"}')).toBe(
      "hello world"
    );
  });

  test("falls back to raw string on invalid JSON", () => {
    expect(extractTextContent("text", "not json")).toBe("not json");
  });

  test("falls back to contentStr when text field missing", () => {
    const raw = '{"other":"val"}';
    expect(extractTextContent("text", raw)).toBe(raw);
  });

  test("extracts post with title and paragraphs", () => {
    const content = JSON.stringify({
      title: "Update",
      en_us: [
        [
          { tag: "text", text: "line one " },
          { tag: "a", text: "link", href: "https://x.com" },
        ],
        [{ tag: "at", user_name: "Alice", user_id: "u1" }],
      ],
    });
    const result = extractTextContent("post", content);
    expect(result).toContain("**Update**");
    expect(result).toContain("line one [link](https://x.com)");
    expect(result).toContain("@Alice");
  });

  test("post falls back to user_id when user_name missing", () => {
    const content = JSON.stringify({
      en_us: [[{ tag: "at", user_id: "u123" }]],
    });
    expect(extractTextContent("post", content)).toContain("@u123");
  });

  test("returns [Image] for image type", () => {
    expect(extractTextContent("image", '{"image_key":"abc"}')).toBe("[Image]");
  });

  test("returns file name for file type", () => {
    expect(extractTextContent("file", '{"file_name":"doc.pdf"}')).toBe(
      "[File: doc.pdf]"
    );
  });

  test("returns unknown for file without name", () => {
    expect(extractTextContent("file", '{"file_key":"abc"}')).toBe(
      "[File: unknown]"
    );
  });

  test("returns card message prefix for interactive", () => {
    const result = extractTextContent(
      "interactive",
      '{"type":"template","data":{}}'
    );
    expect(result).toStartWith("[Card message]");
  });

  test("returns tagged fallback for unknown msg_type", () => {
    const result = extractTextContent("sticker", '{"sticker_id":"abc"}');
    expect(result).toStartWith("[sticker]");
  });

  test("interactive truncates contentStr at 500 chars", () => {
    const longContent = JSON.stringify({ type: "t", data: "x".repeat(600) });
    const result = extractTextContent("interactive", longContent);
    expect(result).toBe("[Card message] " + longContent.slice(0, 500));
  });

  test("unknown msg_type truncates contentStr at 300 chars", () => {
    const longContent = JSON.stringify({ data: "y".repeat(400) });
    const result = extractTextContent("sticker", longContent);
    expect(result).toBe("[sticker] " + longContent.slice(0, 300));
  });

  test("post title appears exactly once, not duplicated by language iteration", () => {
    const content = JSON.stringify({
      title: "Report",
      en_us: [[{ tag: "text", text: "body" }]],
    });
    const result = extractTextContent("post", content);
    expect(result.match(/Report/g)).toHaveLength(1);
  });

  test("post with title but no language arrays", () => {
    const content = JSON.stringify({ title: "Announcement" });
    expect(extractTextContent("post", content)).toBe("**Announcement**");
  });

  test("returns empty string when contentStr is empty", () => {
    expect(extractTextContent("text", "")).toBe("");
  });

  test("post at tag with no user_name or user_id falls back to unknown", () => {
    const content = JSON.stringify({
      en_us: [[{ tag: "at" }]],
    });
    expect(extractTextContent("post", content)).toBe("@unknown");
  });

  test("post with embedded img element degrades gracefully", () => {
    const content = JSON.stringify({
      en_us: [[{ tag: "text", text: "See " }, { tag: "img", image_key: "abc" }]],
    });
    expect(extractTextContent("post", content)).toBe("See ");
  });

  test("post with multiple language keys concatenates all", () => {
    const content = JSON.stringify({
      en_us: [[{ tag: "text", text: "Hello" }]],
      zh_cn: [[{ tag: "text", text: "你好" }]],
    });
    const result = extractTextContent("post", content);
    expect(result).toContain("Hello");
    expect(result).toContain("你好");
  });
});

// ---------------------------------------------------------------------------
// watermarkToStartTime
// ---------------------------------------------------------------------------

describe("watermarkToStartTime", () => {
  test("converts millisecond timestamp to seconds", () => {
    // 1774324217213 ms → 1774324217 s
    expect(watermarkToStartTime("1774324217213")).toBe("1774324217");
  });

  test("leaves seconds timestamp as-is", () => {
    expect(watermarkToStartTime("1774324217")).toBe("1774324217");
  });

  test("handles boundary value (10 digits = seconds)", () => {
    expect(watermarkToStartTime("9999999999")).toBe("9999999999");
  });

  test("converts 11-digit timestamp at boundary", () => {
    expect(watermarkToStartTime("10000000000")).toBe("10000000");
  });

  test("converts real 13-digit millisecond timestamp", () => {
    expect(watermarkToStartTime("1000000000000")).toBe("1000000000");
  });

  test("zero returns zero", () => {
    expect(watermarkToStartTime("0")).toBe("0");
  });

  test("returns non-numeric input unchanged", () => {
    expect(watermarkToStartTime("garbage")).toBe("garbage");
  });
});

// ---------------------------------------------------------------------------
// processItems
// ---------------------------------------------------------------------------

describe("processItems", () => {
  const chat: LarkChat = { chat_id: "oc_test", name: "Test Chat" };

  function makeItem(overrides: Partial<LarkApiItem> & { create_time: string; message_id: string }): LarkApiItem {
    return {
      msg_type: "text",
      sender: { sender_type: "user", sender_id: { open_id: "ou_user1" } },
      body: { content: '{"text":"hello"}' },
      ...overrides,
    };
  }

  test("skips bot messages", () => {
    const items = [
      makeItem({
        message_id: "m1",
        create_time: "1000",
        sender: { sender_type: "app" },
      }),
    ];
    const result = processItems({ chat, items, lastSeen: undefined, allowedSenders: [] });
    expect(result.messages).toHaveLength(0);
  });

  test("skips already-seen messages", () => {
    const items = [
      makeItem({ message_id: "m1", create_time: "500" }),
    ];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages).toHaveLength(0);
  });

  test("skips messages older than watermark", () => {
    const items = [
      makeItem({ message_id: "m1", create_time: "400" }),
    ];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages).toHaveLength(0);
  });

  test("includes messages newer than watermark", () => {
    const items = [
      makeItem({ message_id: "m1", create_time: "600" }),
    ];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages).toHaveLength(1);
    expect(result.messages[0].message_id).toBe("m1");
  });

  test("applies sender gating", () => {
    const items = [
      makeItem({
        message_id: "m1",
        create_time: "600",
        sender: { sender_type: "user", sender_id: { open_id: "ou_blocked" } },
      }),
    ];
    const result = processItems({
      chat,
      items,
      lastSeen: "500",
      allowedSenders: ["ou_allowed"],
    });
    expect(result.messages).toHaveLength(0);
  });

  test("allows message when sender is in allowlist", () => {
    const items = [
      makeItem({
        message_id: "m1",
        create_time: "600",
        sender: { sender_type: "user", sender_id: { open_id: "ou_allowed" } },
      }),
    ];
    const result = processItems({
      chat,
      items,
      lastSeen: "500",
      allowedSenders: ["ou_allowed"],
    });
    expect(result.messages).toHaveLength(1);
  });

  test("empty allowedSenders allows all", () => {
    const items = [
      makeItem({ message_id: "m1", create_time: "600" }),
    ];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages).toHaveLength(1);
  });

  test("computes watermark from all items including bot messages", () => {
    const items = [
      makeItem({
        message_id: "m1",
        create_time: "600",
        sender: { sender_type: "app" },
      }),
      makeItem({ message_id: "m2", create_time: "500" }),
    ];
    const result = processItems({ chat, items, lastSeen: "400", allowedSenders: [] });
    // Watermark should be 600 (from the bot message), even though it's filtered
    expect(result.newWatermark).toBe("600");
  });

  test("returns messages in oldest-first order", () => {
    const items = [
      makeItem({ message_id: "m3", create_time: "800" }),
      makeItem({ message_id: "m2", create_time: "700" }),
      makeItem({ message_id: "m1", create_time: "600" }),
    ];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages.map((m) => m.message_id)).toEqual(["m1", "m2", "m3"]);
  });

  test("falls back to user_id when open_id missing", () => {
    const items = [
      makeItem({
        message_id: "m1",
        create_time: "600",
        sender: { sender_type: "user", sender_id: { user_id: "uid_123" } },
      }),
    ];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages[0].sender_id).toBe("uid_123");
  });

  test("sender_id defaults to unknown when both IDs missing", () => {
    const items = [
      makeItem({
        message_id: "m1",
        create_time: "600",
        sender: { sender_type: "user", sender_id: {} },
      }),
    ];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages[0].sender_id).toBe("unknown");
  });

  test("no items returns undefined watermark", () => {
    const result = processItems({ chat, items: [], lastSeen: "500", allowedSenders: [] });
    expect(result.newWatermark).toBeUndefined();
  });

  test("first poll with no lastSeen includes all user messages", () => {
    const items = [
      makeItem({ message_id: "m1", create_time: "100" }),
      makeItem({ message_id: "m2", create_time: "200" }),
    ];
    const result = processItems({ chat, items, lastSeen: undefined, allowedSenders: [] });
    expect(result.messages).toHaveLength(2);
  });

  test("populates all LarkMessage fields correctly", () => {
    const items = [makeItem({ message_id: "m1", create_time: "600" })];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages[0]).toEqual({
      message_id: "m1",
      chat_id: "oc_test",
      chat_name: "Test Chat",
      sender_id: "ou_user1",
      sender_type: "user",
      msg_type: "text",
      create_time: "600",
      body: "hello",
    });
  });

  test("handles item with missing body gracefully", () => {
    const items = [makeItem({ message_id: "m1", create_time: "600", body: undefined })];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages[0].body).toBe("");
  });

  test("handles item with no sender object", () => {
    const items = [makeItem({ message_id: "m1", create_time: "600", sender: undefined })];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages[0].sender_id).toBe("unknown");
    expect(result.messages[0].sender_type).toBe("user");
  });

  test("numeric comparison handles different digit lengths correctly", () => {
    const items = [makeItem({ message_id: "m1", create_time: "10000" })];
    const result = processItems({ chat, items, lastSeen: "9999", allowedSenders: [] });
    expect(result.messages).toHaveLength(1);
  });

  test("watermark stays at lastSeen when all items are older", () => {
    const items = [makeItem({ message_id: "m1", create_time: "300", sender: { sender_type: "app" } })];
    expect(processItems({ chat, items, lastSeen: "500", allowedSenders: [] }).newWatermark).toBe("500");
  });

  test("watermark floors at lastSeen for older user items too", () => {
    const items = [makeItem({ message_id: "m1", create_time: "300" })];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages).toHaveLength(0);
    expect(result.newWatermark).toBe("500");
  });

  test("sender with sender_type but no sender_id key", () => {
    const items = [makeItem({ message_id: "m1", create_time: "600", sender: { sender_type: "user" } })];
    expect(processItems({ chat, items, lastSeen: "500", allowedSenders: [] }).messages[0].sender_id).toBe("unknown");
  });

  test("non-standard sender_type passes through", () => {
    const items = [makeItem({ message_id: "m1", create_time: "600", sender: { sender_type: "webhook", sender_id: { open_id: "ou_x" } } })];
    expect(processItems({ chat, items, lastSeen: "500", allowedSenders: [] }).messages[0].sender_type).toBe("webhook");
  });

  test("create_time 0 with no lastSeen is included", () => {
    const items = [makeItem({ message_id: "m1", create_time: "0" })];
    const result = processItems({ chat, items, lastSeen: undefined, allowedSenders: [] });
    expect(result.messages).toHaveLength(1);
    expect(result.newWatermark).toBe("0");
  });

  test("skips items with NaN create_time", () => {
    const items = [makeItem({ message_id: "m1", create_time: "bad" })];
    const result = processItems({ chat, items, lastSeen: undefined, allowedSenders: [] });
    expect(result.messages).toHaveLength(0);
  });

  test("NaN create_time item does not affect watermark", () => {
    const items = [
      makeItem({ message_id: "m1", create_time: "bad" }),
      makeItem({ message_id: "m2", create_time: "600" }),
    ];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages).toHaveLength(1);
    expect(result.newWatermark).toBe("600");
  });

  test("watermark advances past lastSeen when only a bot item is newer", () => {
    const items = [
      makeItem({ message_id: "m1", create_time: "300" }),
      makeItem({ message_id: "m2", create_time: "700", sender: { sender_type: "app" } }),
    ];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: [] });
    expect(result.messages).toHaveLength(0);
    expect(result.newWatermark).toBe("700");
  });

  test("sender gating blocks unknown sender when not in allowlist", () => {
    const items = [makeItem({ message_id: "m1", create_time: "600", sender: { sender_type: "user", sender_id: {} } })];
    const result = processItems({ chat, items, lastSeen: "500", allowedSenders: ["ou_specific"] });
    expect(result.messages).toHaveLength(0);
  });

  test("returns oldest-first even when input is already oldest-first", () => {
    const items = [
      makeItem({ message_id: "m1", create_time: "600" }),
      makeItem({ message_id: "m2", create_time: "700" }),
    ];
    const ids = processItems({ chat, items, lastSeen: "500", allowedSenders: [] }).messages.map(m => m.message_id);
    expect(ids).toEqual(["m1", "m2"]);
  });
});

// ---------------------------------------------------------------------------
// parsePermissionVerdict
// ---------------------------------------------------------------------------

describe("parsePermissionVerdict", () => {
  test("parses 'yes abcde'", () => {
    const r = parsePermissionVerdict("yes abcde");
    expect(r).toEqual({ request_id: "abcde", behavior: "allow" });
  });

  test("parses 'no fghij'", () => {
    const r = parsePermissionVerdict("no fghij");
    expect(r).toEqual({ request_id: "fghij", behavior: "deny" });
  });

  test("parses 'y abcde' shorthand", () => {
    const r = parsePermissionVerdict("y abcde");
    expect(r).toEqual({ request_id: "abcde", behavior: "allow" });
  });

  test("parses 'n abcde' shorthand", () => {
    const r = parsePermissionVerdict("n abcde");
    expect(r).toEqual({ request_id: "abcde", behavior: "deny" });
  });

  test("handles case insensitivity (phone autocorrect)", () => {
    const r = parsePermissionVerdict("Yes ABCDE");
    expect(r).toEqual({ request_id: "abcde", behavior: "allow" });
  });

  test("handles leading/trailing whitespace", () => {
    const r = parsePermissionVerdict("  yes abcde  ");
    expect(r).toEqual({ request_id: "abcde", behavior: "allow" });
  });

  test("rejects IDs containing 'l' (excluded from alphabet)", () => {
    expect(parsePermissionVerdict("yes abcle")).toBeNull();
  });

  test("rejects IDs containing digits", () => {
    expect(parsePermissionVerdict("yes abc1e")).toBeNull();
  });

  test("rejects IDs with wrong length", () => {
    expect(parsePermissionVerdict("yes abc")).toBeNull();
    expect(parsePermissionVerdict("yes abcdef")).toBeNull();
  });

  test("rejects non-verdict text", () => {
    expect(parsePermissionVerdict("hello world")).toBeNull();
    expect(parsePermissionVerdict("approve it")).toBeNull();
    expect(parsePermissionVerdict("yes")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// buildRichTextContent
// ---------------------------------------------------------------------------

describe("buildRichTextContent", () => {
  test("creates post content with title", () => {
    const result = buildRichTextContent("Line 1\nLine 2", "My Title");
    expect(result).toEqual({
      en_us: {
        title: "My Title",
        content: [
          [{ tag: "text", text: "Line 1" }],
          [{ tag: "text", text: "Line 2" }],
        ],
      },
    });
  });

  test("defaults title to empty string", () => {
    const result = buildRichTextContent("Hello");
    expect((result as any).en_us.title).toBe("");
  });

  test("handles single line", () => {
    const result = buildRichTextContent("single");
    expect((result as any).en_us.content).toHaveLength(1);
  });

  test("handles empty string", () => {
    const result = buildRichTextContent("");
    expect((result as any).en_us.content).toEqual([[{ tag: "text", text: "" }]]);
  });

  test("trailing newline creates empty paragraph", () => {
    const result = buildRichTextContent("Hello\n");
    expect((result as any).en_us.content).toEqual([
      [{ tag: "text", text: "Hello" }],
      [{ tag: "text", text: "" }],
    ]);
  });
});

// ---------------------------------------------------------------------------
// formatPermissionPrompt
// ---------------------------------------------------------------------------

describe("formatPermissionPrompt", () => {
  test("includes all fields with action first", () => {
    const result = formatPermissionPrompt({
      request_id: "abcde",
      tool_name: "Bash",
      description: "Run a command",
      input_preview: '{"command":"ls"}',
    });
    expect(result).toContain("Permission Request");
    expect(result).toContain("Bash");
    expect(result).toContain("Run a command");
    expect(result).toContain('{"command":"ls"}');
    expect(result).toContain('"yes abcde"');
    expect(result).toContain('"no abcde"');
    // Action (reply instruction) should appear before tool details
    const replyIdx = result.indexOf("Reply");
    const toolIdx = result.indexOf("Tool:");
    expect(replyIdx).toBeLessThan(toolIdx);
  });

  test("truncates long input_preview with ellipsis", () => {
    const longPreview = "x".repeat(300);
    const result = formatPermissionPrompt({
      request_id: "abcde",
      tool_name: "Write",
      description: "Write a file",
      input_preview: longPreview,
    });
    expect(result).toContain("x".repeat(150) + "...");
    expect(result).not.toContain("x".repeat(151) + ".");
  });

  test("150 chars exactly: no truncation", () => {
    const preview = "x".repeat(150);
    const result = formatPermissionPrompt({ request_id: "abcde", tool_name: "T", description: "d", input_preview: preview });
    expect(result).toContain(preview);
    expect(result).not.toContain("...");
  });

  test("151 chars: triggers truncation", () => {
    const preview = "x".repeat(151);
    const result = formatPermissionPrompt({ request_id: "abcde", tool_name: "T", description: "d", input_preview: preview });
    expect(result).toContain("x".repeat(150) + "...");
  });

  test("no ellipsis when preview fits", () => {
    const result = formatPermissionPrompt({
      request_id: "abcde",
      tool_name: "Bash",
      description: "test",
      input_preview: "short",
    });
    expect(result).toContain("short");
    expect(result).not.toContain("...");
  });
});

// ---------------------------------------------------------------------------
// parseSlashCommand
// ---------------------------------------------------------------------------

describe("parseSlashCommand", () => {
  test("parses simple command", () => {
    const cmd = parseSlashCommand("/clear");
    expect(cmd).toEqual({ name: "clear", args: "", raw: "/clear" });
  });

  test("parses command with args", () => {
    const cmd = parseSlashCommand("/model sonnet");
    expect(cmd).toEqual({ name: "model", args: "sonnet", raw: "/model sonnet" });
  });

  test("parses hyphenated command", () => {
    const cmd = parseSlashCommand("/help-channel");
    expect(cmd).toEqual({ name: "help-channel", args: "", raw: "/help-channel" });
  });

  test("normalizes to lowercase", () => {
    const cmd = parseSlashCommand("/CLEAR");
    expect(cmd).not.toBeNull();
    expect(cmd!.name).toBe("clear");
  });

  test("trims surrounding whitespace", () => {
    const cmd = parseSlashCommand("  /status  ");
    expect(cmd).toEqual({ name: "status", args: "", raw: "/status" });
  });

  test("trims arg whitespace", () => {
    const cmd = parseSlashCommand("/model   sonnet  ");
    expect(cmd).not.toBeNull();
    expect(cmd!.args).toBe("sonnet");
  });

  test("returns null for non-command text", () => {
    expect(parseSlashCommand("hello world")).toBeNull();
  });

  test("returns null for empty string", () => {
    expect(parseSlashCommand("")).toBeNull();
  });

  test("returns null for bare slash", () => {
    expect(parseSlashCommand("/")).toBeNull();
  });

  test("returns null for slash with only spaces", () => {
    expect(parseSlashCommand("/ ")).toBeNull();
  });

  test("returns null for slash with number start", () => {
    expect(parseSlashCommand("/123")).toBeNull();
  });

  test("preserves multi-word args", () => {
    const cmd = parseSlashCommand("/repo internal-affairs feat/new-thing");
    expect(cmd).not.toBeNull();
    expect(cmd!.name).toBe("repo");
    expect(cmd!.args).toBe("internal-affairs feat/new-thing");
  });
});

// ---------------------------------------------------------------------------
// routeCommand
// ---------------------------------------------------------------------------

describe("routeCommand", () => {
  test("routes Claude built-in /clear as forward", () => {
    const route = routeCommand({ name: "clear", args: "", raw: "/clear" });
    expect(route).toEqual({ kind: "forward" });
  });

  test("routes Claude built-in /compact as forward", () => {
    const route = routeCommand({ name: "compact", args: "", raw: "/compact" });
    expect(route).toEqual({ kind: "forward" });
  });

  test("routes Claude built-in /model as forward", () => {
    const route = routeCommand({ name: "model", args: "sonnet", raw: "/model sonnet" });
    expect(route).toEqual({ kind: "forward" });
  });

  test("routes /status as mcp", () => {
    const route = routeCommand({ name: "status", args: "", raw: "/status" });
    expect(route).toEqual({ kind: "mcp", handler: "status" });
  });

  test("routes /help-channel as mcp", () => {
    const route = routeCommand({ name: "help-channel", args: "", raw: "/help-channel" });
    expect(route).toEqual({ kind: "mcp", handler: "help-channel" });
  });

  test("routes unknown command as unknown with error text", () => {
    const route = routeCommand({ name: "nonexistent", args: "", raw: "/nonexistent" });
    expect(route.kind).toBe("unknown");
    expect((route as { kind: "unknown"; text: string }).text).toContain("/nonexistent");
    expect((route as { kind: "unknown"; text: string }).text).toContain("Available commands");
  });

  test("all Claude builtins route as forward", () => {
    const builtins = [
      "clear", "compact", "config", "cost", "doctor", "help", "login",
      "logout", "memory", "model", "permissions", "review", "terminal-setup", "vim",
    ];
    for (const name of builtins) {
      const route = routeCommand({ name, args: "", raw: `/${name}` });
      expect(route.kind).toBe("forward");
    }
  });
});

// ---------------------------------------------------------------------------
// getAvailableCommands
// ---------------------------------------------------------------------------

describe("getAvailableCommands", () => {
  test("returns non-empty array", () => {
    const cmds = getAvailableCommands();
    expect(cmds.length).toBeGreaterThan(0);
  });

  test("contains both categories", () => {
    const cmds = getAvailableCommands();
    const categories = new Set(cmds.map((c) => c.category));
    expect(categories.has("claude")).toBe(true);
    expect(categories.has("mcp")).toBe(true);
  });

  test("every command has name and description", () => {
    for (const cmd of getAvailableCommands()) {
      expect(cmd.name.length).toBeGreaterThan(0);
      expect(cmd.description.length).toBeGreaterThan(0);
    }
  });

  test("mcp commands listed first", () => {
    const cmds = getAvailableCommands();
    expect(cmds[0].category).toBe("mcp");
  });
});

// ---------------------------------------------------------------------------
// formatUnknownCommandReply
// ---------------------------------------------------------------------------

describe("formatUnknownCommandReply", () => {
  test("contains the unknown command name", () => {
    const result = formatUnknownCommandReply("foo", getAvailableCommands());
    expect(result).toContain("/foo");
    expect(result).toContain("Unknown command");
  });

  test("lists available commands", () => {
    const result = formatUnknownCommandReply("foo", getAvailableCommands());
    expect(result).toContain("/status");
    expect(result).toContain("/clear");
  });
});

// ---------------------------------------------------------------------------
// formatStatusReply
// ---------------------------------------------------------------------------

describe("formatStatusReply", () => {
  const info: StatusInfo = {
    uptimeSeconds: 3661,
    monitoredChats: ["oc_abc123"],
    pollIntervalMs: 5000,
    lastPollTimes: { oc_abc123: "1774324217213" },
  };

  test("contains uptime in human-readable format", () => {
    const result = formatStatusReply(info);
    expect(result).toContain("1h 1m 1s");
  });

  test("contains poll interval", () => {
    const result = formatStatusReply(info);
    expect(result).toContain("5000ms");
  });

  test("contains monitored chat IDs", () => {
    const result = formatStatusReply(info);
    expect(result).toContain("oc_abc123");
  });

  test("handles short uptime (minutes only)", () => {
    const result = formatStatusReply({ ...info, uptimeSeconds: 125 });
    expect(result).toContain("2m 5s");
  });

  test("handles very short uptime (seconds only)", () => {
    const result = formatStatusReply({ ...info, uptimeSeconds: 45 });
    expect(result).toContain("45s");
  });

  test("handles empty lastPollTimes", () => {
    const result = formatStatusReply({ ...info, lastPollTimes: {} });
    expect(result).not.toContain("Last seen:");
  });
});

// ---------------------------------------------------------------------------
// formatHelpChannelReply
// ---------------------------------------------------------------------------

describe("formatHelpChannelReply", () => {
  test("groups commands by category", () => {
    const result = formatHelpChannelReply(getAvailableCommands());
    expect(result).toContain("Channel commands:");
    expect(result).toContain("Claude Code commands");
  });

  test("lists mcp commands with descriptions", () => {
    const result = formatHelpChannelReply(getAvailableCommands());
    expect(result).toContain("/status");
    expect(result).toContain("/help-channel");
  });

  test("lists Claude built-ins", () => {
    const result = formatHelpChannelReply(getAvailableCommands());
    expect(result).toContain("/clear");
    expect(result).toContain("/compact");
  });
});

// ---------------------------------------------------------------------------
// routeSupervisorCommand
// ---------------------------------------------------------------------------

describe("routeSupervisorCommand", () => {
  test("routes /start as lifecycle", () => {
    const route = routeSupervisorCommand({ name: "start", args: "internal-affairs", raw: "/start internal-affairs" });
    expect(route).toEqual({ kind: "lifecycle", command: "start" });
  });

  test("routes /stop as lifecycle", () => {
    const route = routeSupervisorCommand({ name: "stop", args: "", raw: "/stop" });
    expect(route).toEqual({ kind: "lifecycle", command: "stop" });
  });

  test("routes /restart as lifecycle", () => {
    const route = routeSupervisorCommand({ name: "restart", args: "", raw: "/restart" });
    expect(route).toEqual({ kind: "lifecycle", command: "restart" });
  });

  test("routes /status as query", () => {
    const route = routeSupervisorCommand({ name: "status", args: "", raw: "/status" });
    expect(route).toEqual({ kind: "query", command: "status" });
  });

  test("routes /help-channel as query", () => {
    const route = routeSupervisorCommand({ name: "help-channel", args: "", raw: "/help-channel" });
    expect(route).toEqual({ kind: "query", command: "help-channel" });
  });

  test("routes /clear as passthrough", () => {
    const route = routeSupervisorCommand({ name: "clear", args: "", raw: "/clear" });
    expect(route).toEqual({ kind: "passthrough" });
  });

  test("routes /compact as passthrough", () => {
    const route = routeSupervisorCommand({ name: "compact", args: "", raw: "/compact" });
    expect(route).toEqual({ kind: "passthrough" });
  });

  test("routes unknown command as unknown", () => {
    const route = routeSupervisorCommand({ name: "foo", args: "", raw: "/foo" });
    expect(route.kind).toBe("unknown");
  });
});

// ---------------------------------------------------------------------------
// validateRepoName
// ---------------------------------------------------------------------------

describe("validateRepoName", () => {
  const available = ["internal-affairs", "person-service", "kelola-app"];

  test("returns valid repo name", () => {
    expect(validateRepoName("internal-affairs", available)).toBe("internal-affairs");
  });

  test("returns null for unknown repo", () => {
    expect(validateRepoName("nonexistent", available)).toBeNull();
  });

  test("strips path separators", () => {
    expect(validateRepoName("../../../etc/passwd", available)).toBeNull();
  });

  test("strips tildes", () => {
    expect(validateRepoName("~root", available)).toBeNull();
  });

  test("strips dots", () => {
    expect(validateRepoName("..internal-affairs", available)).toBeNull();
  });

  test("returns null for empty string", () => {
    expect(validateRepoName("", available)).toBeNull();
  });

  test("returns null for only special chars", () => {
    expect(validateRepoName("../../../", available)).toBeNull();
  });

  test("allows hyphens and underscores", () => {
    expect(validateRepoName("person-service", available)).toBe("person-service");
  });

  test("is case-sensitive", () => {
    expect(validateRepoName("Internal-Affairs", available)).toBeNull();
  });
});
