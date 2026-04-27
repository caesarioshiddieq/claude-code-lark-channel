#!/usr/bin/env bash
# infra/headroom-check.sh
#
# Read-only health + headroom report for claude-code-vm. Answers:
#   "Should we bump MAX_CONCURRENT_SPAWNS_GLOBAL from 1 to 2?"
#
# Replaces M1b Prometheus /metrics for the immediate N=2 decision.
# Safe to run anytime: PRAGMA query_only=1 on the live SQLite DB.
#
# Usage:  bash infra/headroom-check.sh
# Env overrides: PROJECT, VM, ZONE, DB_PATH

set -euo pipefail

PROJECT="${PROJECT:-devsecops-480902}"
VM="${VM:-claude-code-vm}"
ZONE="${ZONE:-asia-southeast2-b}"
DB_PATH="${DB_PATH:-/var/lib/claude-vm/queue.db}"

remote_python() {
cat <<'PYEOF'
import os, sqlite3, subprocess

db = sqlite3.connect(os.environ.get("DB_PATH", "/var/lib/claude-vm/queue.db"))
db.execute("PRAGMA query_only=1")

def section(title):
    print(f"\n== {title} ==")

def run(sql):
    cur = db.execute(sql)
    cols = [d[0] for d in cur.description]
    rows = cur.fetchall()
    if not rows:
        print("  (no rows)")
        return
    widths = [max(len(c), max((len(str(r[i])) if r[i] is not None else 1) for r in rows)) for i, c in enumerate(cols)]
    fmt = " | ".join("{:<" + str(w) + "}" for w in widths)
    print(fmt.format(*cols))
    print("-+-".join("-" * w for w in widths))
    for r in rows:
        print(fmt.format(*[("-" if v is None else str(v)) for v in r]))

section("Q1: Spawn rate + cumulative (turn_usage)")
run("""
  SELECT
    SUM(CASE WHEN created_at > unixepoch()-3600  THEN 1 ELSE 0 END) AS spawns_1h,
    SUM(CASE WHEN created_at > unixepoch()-86400 THEN 1 ELSE 0 END) AS spawns_24h,
    COUNT(*)                                                        AS spawns_total,
    ROUND(AVG(input_tokens + output_tokens), 0)                     AS avg_tokens_per_spawn,
    SUM(is_rate_limit_error)                                        AS rate_limit_hits_total
  FROM turn_usage
""")

section("Q2: Hourly token volume (last 24h)")
run("""
  SELECT
    strftime('%Y-%m-%d %H:00', created_at, 'unixepoch', 'localtime') AS hour,
    COUNT(*)                                                          AS spawns,
    SUM(input_tokens + output_tokens)                                 AS tokens,
    SUM(cache_read_tokens)                                            AS cache_read,
    SUM(cache_creation_tokens)                                        AS cache_create
  FROM turn_usage
  WHERE created_at > unixepoch() - 86400
  GROUP BY hour ORDER BY hour DESC
""")

section("Q3: Per-task pending demand (concurrency need)")
run("""
  SELECT
    task_id,
    COUNT(*)                                              AS pending,
    MIN(created_at)                                       AS oldest_pending_ms,
    SUM(CASE WHEN defer_count > 0 THEN 1 ELSE 0 END)      AS deferred,
    SUM(CASE WHEN phase != 'normal' THEN 1 ELSE 0 END)    AS non_normal_phase
  FROM inbox
  WHERE processed_at IS NULL
  GROUP BY task_id ORDER BY pending DESC LIMIT 20
""")

section("Q4: Backlog age")
run("""
  SELECT
    COUNT(*)                                                                              AS total_pending,
    SUM(CASE WHEN created_at < (unixepoch()-300)*1000   THEN 1 ELSE 0 END)                AS older_than_5min,
    SUM(CASE WHEN created_at < (unixepoch()-3600)*1000  THEN 1 ELSE 0 END)                AS older_than_1h,
    SUM(CASE WHEN scheduled_for IS NOT NULL AND scheduled_for > unixepoch()*1000 THEN 1 ELSE 0 END) AS deferred_now
  FROM inbox WHERE processed_at IS NULL
""")

section("Q5: DLQ growth (failure rate)")
run("""
  SELECT
    COUNT(*)                                                          AS dlq_total,
    SUM(CASE WHEN moved_at > unixepoch()-3600  THEN 1 ELSE 0 END)     AS dlq_1h,
    SUM(CASE WHEN moved_at > unixepoch()-86400 THEN 1 ELSE 0 END)     AS dlq_24h
  FROM dlq
""")

section("Q6: Phase distribution (last 24h)")
run("""
  SELECT
    phase,
    COUNT(*)                                  AS spawns_24h,
    ROUND(AVG(input_tokens + output_tokens),0) AS avg_tokens,
    ROUND(AVG(cache_read_tokens),0)            AS avg_cache_read
  FROM turn_usage
  WHERE created_at > unixepoch() - 86400
  GROUP BY phase
""")

section("Sessions overview")
run("""
  SELECT
    COUNT(*)                                                          AS sessions_total,
    SUM(CASE WHEN last_active > unixepoch()-86400 THEN 1 ELSE 0 END)  AS active_24h,
    MAX(turn_count)                                                   AS max_turns,
    ROUND(AVG(turn_count), 1)                                          AS avg_turns
  FROM sessions
""")

section("Memory snapshot (live)")
sup = subprocess.run(
    ["bash","-c","ps -o pid=,rss=,vsz=,cmd= -p $(pgrep -f claude-vm-supervisor) 2>/dev/null || echo 'supervisor not running'"],
    capture_output=True, text=True)
print("supervisor:", sup.stdout.strip())

children = subprocess.run(
    ["bash","-c",
     "ps -eo rss=,cmd= | grep -E 'claude.*-p' | grep -v grep | "
     "awk '{rss+=$1; n++} END {if (n>0) printf \"claude -p children: %d procs, %.1f MB total RSS\\n\", n, rss/1024; else print \"claude -p children: none running\"}'"],
    capture_output=True, text=True)
print(children.stdout.strip())

mem = subprocess.run(
    ["bash","-c","free -m | awk '/^Mem:/{printf \"system: total=%d MB, used=%d MB, available=%d MB\\n\", $2, $3, $7}'"],
    capture_output=True, text=True)
print(mem.stdout.strip())

print("\n== N=2 verdict cheatsheet ==")
print("  GO  : Q1 spawns_1h<30, Q3 max-pending<3 across >=2 tasks, Q4 older_than_5min=0, Q5 dlq_1h=0, children<1.5GB")
print("  HOLD: any red — fix root cause first, M1b does not help")
PYEOF
}

echo "[*] Connecting to ${VM} (project=${PROJECT}, zone=${ZONE})..."
remote_python | gcloud compute ssh "${VM}" \
  --tunnel-through-iap \
  --project="${PROJECT}" \
  --zone="${ZONE}" \
  -- "DB_PATH='${DB_PATH}' python3 -"
