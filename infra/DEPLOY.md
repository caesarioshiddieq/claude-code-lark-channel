# Deployment Guide

## Prerequisites

- Go 1.26+ (for building on the build host)
- `claude` CLI installed and authenticated on the target VM
- systemd (target VM is a GCE COS/Debian instance)

## 1. Build

Run on your build host (or the VM if Go is installed there):

```bash
cd ~/Kerja/claude-code-lark-channel
GOOS=linux GOARCH=amd64 go build -o infra/claude-vm-supervisor ./cmd/supervisor/
```

## 2. First-Time Setup on VM

```bash
# Create data directories
sudo mkdir -p /var/lib/claude-vm/sessions
sudo chown -R caesario:caesario /var/lib/claude-vm

# Create config directory and populate env file
mkdir -p /home/caesario/bin ~/.config/claude-vm
cp infra/env.example ~/.config/claude-vm/env
chmod 600 ~/.config/claude-vm/env   # restrict: file contains LARK_APP_SECRET
# Edit the env file — at minimum set LARK_APP_SECRET and LARK_TASKLIST_ID
# SECURITY: set CLAUDE_ALLOW_LIST to your operator Lark open_ids before production.
# An empty value allows ALL Lark users to invoke Claude on this VM.
$EDITOR ~/.config/claude-vm/env

# Deploy binary and service unit
cp infra/claude-vm-supervisor /home/caesario/bin/
sudo cp infra/claude-vm.service /etc/systemd/system/

# Enable and start
sudo systemctl daemon-reload
sudo systemctl enable claude-vm
sudo systemctl start claude-vm
sudo systemctl status claude-vm
```

## 3. Update Existing Deploy (Binary Only)

```bash
sudo systemctl stop claude-vm
cp infra/claude-vm-supervisor /home/caesario/bin/
sudo systemctl start claude-vm
```

## 4. Logs

```bash
journalctl -u claude-vm -f
```

## 4b. Headroom Check (read-only)

```bash
bash infra/headroom-check.sh
```

Runs SQLite read-only queries (`PRAGMA query_only=1`) against `/var/lib/claude-vm/queue.db` plus a memory snapshot. Use before bumping `MAX_CONCURRENT_SPAWNS_GLOBAL`. The script's tail prints a GO/HOLD cheatsheet against thresholds for spawn rate, per-task pending, backlog age, DLQ growth, and child-process RSS.

## 5. Verification Scenarios

Run these against a live deployment to confirm correct end-to-end behaviour.

### 5.1 Happy Path + Session Continuity
1. Create a Lark task named "Test 1".
2. Post comment: `hello, remember 42`.
3. Verify Claude replies within 60 s.
4. Post follow-up: `what number?`.
5. Verify reply contains `42` (proves `--resume` continuity across turns).

### 5.2 Idempotency (SIGKILL Recovery)
1. Send a comment, then `sudo kill -9 $(pidof claude-vm-supervisor)` while a reply is in flight.
2. Restart the service: `sudo systemctl start claude-vm`.
3. Verify no duplicate Lark comments appear; the `outbox` row has a non-null `lark_comment_id`.

### 5.3 Concurrent Input (Order Preservation)
1. Post 3 comments on the same task within 1 s.
2. Verify the Claude transcript shows 3 turns in submission order with no corruption.

### 5.4 Echo Filter
```bash
journalctl -u claude-vm | grep "enqueue"
```
Confirm the bot's own reply comments do **not** appear in enqueue log lines.

### 5.5 Watermark Resumption
1. `sudo systemctl stop claude-vm`.
2. Post 2 comments in Lark while the service is down.
3. `sudo systemctl start claude-vm`.
4. Verify both comments are processed (watermark advanced correctly).

### 5.6 Spot Preemption Simulation
```bash
gcloud compute instances simulate-maintenance-event claude-code-vm \
  --zone=asia-southeast2-b
```
After the instance restarts, verify:
- Inbox rows from before preemption are replayed.
- No duplicate replies are posted (outbox dedup gate holds).

### 5.7 Cross-Task Isolation
1. Create 2 Lark tasks.
2. Post interleaved comments on both tasks.
3. SSH to VM and verify separate Claude transcripts exist under `~/.claude/projects/`.
