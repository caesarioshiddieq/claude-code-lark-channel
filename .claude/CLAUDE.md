# claude-code-lark-channel

## Overview
Lark Task v2 multi-session supervisor for claude-code-vm (GCE Spot e2-medium, asia-southeast2-b).
Go supervisor replaces legacy TypeScript supervisor.ts (screen-based, single session).

## Go conventions
- `go test -race ./...` always with race detector
- `go vet ./...` before commit
- SQLite via `modernc.org/sqlite` (pure Go, no CGO)
- flock via `syscall.Flock` (Linux stdlib)

## Key env vars (Go supervisor)
LARK_APP_ID, LARK_APP_SECRET, LARK_BASE_URL — Lark auth
LARK_TASKLIST_ID — GUID of tasklist to poll
CLAUDE_ALLOW_LIST — comma-separated allowed Lark user open_ids
DB_PATH — SQLite file (default: /var/lib/claude-vm/queue.db)
LOCK_DIR — flock dir (default: /var/lib/claude-vm/sessions)
POLL_INTERVAL — poll interval (default: 30s)
MAX_TURNS_PER_SESSION — turns before /compact injection (default: 50)
