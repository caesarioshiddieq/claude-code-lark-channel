package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/echo"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/lark"
	q "github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/worker"
)

type config struct {
	AppID              string
	AppSecret          string
	BaseURL            string
	TasklistID         string
	AllowList          []string
	DBPath             string
	PollInterval       time.Duration
	MaxTurnsPerSession int
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}

func loadConfig() config {
	pollInterval := 30 * time.Second
	if s := os.Getenv("POLL_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			pollInterval = d
		} else {
			log.Printf("invalid POLL_INTERVAL=%q: %v; using default 30s", s, err)
		}
	}
	dbPath := "/var/lib/claude-vm/queue.db"
	if s := os.Getenv("DB_PATH"); s != "" {
		dbPath = s
	}
	var allowList []string
	if s := os.Getenv("CLAUDE_ALLOW_LIST"); s != "" {
		for _, id := range strings.Split(s, ",") {
			if t := strings.TrimSpace(id); t != "" {
				allowList = append(allowList, t)
			}
		}
	}
	maxTurns := 50
	if s := os.Getenv("MAX_TURNS_PER_SESSION"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			maxTurns = n
		} else {
			log.Printf("invalid MAX_TURNS_PER_SESSION=%q, using default 50", s)
		}
	}
	return config{
		AppID:              mustEnv("LARK_APP_ID"),
		AppSecret:          mustEnv("LARK_APP_SECRET"),
		BaseURL:            mustEnv("LARK_BASE_URL"),
		TasklistID:         mustEnv("LARK_TASKLIST_ID"),
		AllowList:          allowList,
		DBPath:             dbPath,
		PollInterval:       pollInterval,
		MaxTurnsPerSession: maxTurns,
	}
}

func isAllowed(creatorID string, allowList []string) bool {
	if len(allowList) == 0 {
		return true
	}
	for _, id := range allowList {
		if id == creatorID {
			return true
		}
	}
	return false
}

func pollOnce(ctx context.Context, client *lark.Client, db *q.DB, cfg config) {
	tasks, err := client.ListTasklistTasks(ctx, cfg.TasklistID)
	if err != nil {
		log.Printf("[poller] list tasks: %v", err)
		return
	}
	for _, taskID := range tasks {
		if ctx.Err() != nil {
			return
		}
		pollTask(ctx, client, db, taskID, cfg.AllowList)
	}
}

func pollTask(ctx context.Context, client *lark.Client, db *q.DB, taskID string, allowList []string) {
	watermark, _, err := db.GetWatermark(ctx, taskID)
	if err != nil {
		log.Printf("[poller] get watermark task=%s: %v", taskID, err)
		return
	}
	pageToken := ""
	var latestID string
	for {
		resp, err := client.ListComments(ctx, taskID, pageToken)
		if err != nil {
			log.Printf("[poller] list comments task=%s: %v", taskID, err)
			break
		}
		for _, c := range resp.Items {
			if echo.IsEchoComment(c) {
				continue
			}
			if !isAllowed(c.Creator.ID, allowList) {
				continue
			}
			if watermark != "" && c.CommentID <= watermark {
				continue
			}
			if err := db.InsertInbox(ctx, taskID, c); err != nil {
				log.Printf("[poller] insert inbox task=%s comment=%s: %v", taskID, c.CommentID, err)
			}
			if latestID == "" || c.CommentID > latestID {
				latestID = c.CommentID
			}
		}
		if !resp.HasMore {
			break
		}
		pageToken = resp.PageToken
	}
	if latestID != "" {
		if err := db.SetWatermark(ctx, taskID, latestID); err != nil {
			log.Printf("[poller] set watermark task=%s: %v", taskID, err)
		}
	}
}

func processOne(ctx context.Context, db *q.DB, client *lark.Client, maxTurns int) bool {
	row, found, err := db.NextInboxRow(ctx)
	if err != nil {
		log.Printf("[worker] next inbox: %v", err)
		return false
	}
	if !found {
		return false
	}

	lockFile, err := worker.LockTask(row.TaskID)
	if err != nil {
		log.Printf("[worker] lock task=%s: %v", row.TaskID, err)
		return false
	}
	defer worker.UnlockTask(lockFile)

	sessionUUID, found, err := db.GetSession(ctx, row.TaskID)
	if err != nil {
		log.Printf("[worker] get session task=%s: %v", row.TaskID, err)
		return false
	}
	isNew := !found
	if isNew {
		sessionUUID = newUUID()
		if err := db.UpsertSession(ctx, row.TaskID, sessionUUID); err != nil {
			log.Printf("[worker] upsert session task=%s: %v", row.TaskID, err)
			return false
		}
	}

	prompt := row.Content
	turns, err := db.GetTurnCount(ctx, row.TaskID)
	if err != nil {
		log.Printf("[worker] get turn count task=%s: %v", row.TaskID, err)
	}
	if turns > 0 && turns%maxTurns == 0 {
		prompt = "/compact\n\n" + prompt
	}

	reply, err := worker.SpawnClaude(ctx, sessionUUID, isNew, prompt)
	if err != nil {
		if ctx.Err() != nil {
			// Graceful shutdown: leave row in inbox for next startup to retry.
			log.Printf("[worker] spawn cancelled (shutdown), leaving task=%s comment=%s in inbox", row.TaskID, row.CommentID)
			return false
		}
		log.Printf("[worker] spawn task=%s comment=%s: %v", row.TaskID, row.CommentID, err)
		if dlqErr := db.MoveToDeadLetter(ctx, row.CommentID, row.TaskID, err.Error()); dlqErr != nil {
			log.Printf("[worker] dlq move failed task=%s comment=%s: %v; marking processed", row.TaskID, row.CommentID, dlqErr)
			if mErr := db.MarkInboxProcessed(ctx, row.CommentID); mErr != nil {
				log.Printf("[worker] mark processed fallback failed comment=%s: %v", row.CommentID, mErr)
			}
		}
		return true
	}

	hash := worker.ContentHash(row.TaskID, reply)
	existingID, found, outboxErr := db.OutboxCheck(ctx, hash)
	if outboxErr != nil {
		log.Printf("[worker] outbox check task=%s: %v", row.TaskID, outboxErr)
	}
	if found {
		if existingID != "" {
			log.Printf("[worker] reply already posted (outbox hit) task=%s", row.TaskID)
		} else {
			// Crash-recovery: hash recorded but lark_comment_id not confirmed; re-post.
			log.Printf("[worker] outbox half-posted state (re-posting) task=%s", row.TaskID)
			if newCommentID, err := client.PostComment(ctx, row.TaskID, reply, row.CommentID); err != nil {
				log.Printf("[worker] post comment task=%s: %v", row.TaskID, err)
			} else {
				if err := db.OutboxMarkPosted(ctx, hash, newCommentID); err != nil {
					log.Printf("[worker] outbox mark posted task=%s: %v", row.TaskID, err)
				}
			}
		}
	} else {
		// Write-before-post: record hash before calling Lark to enable dedup on retry.
		if err := db.OutboxInsert(ctx, hash, row.TaskID, row.CommentID); err != nil {
			log.Printf("[worker] outbox insert task=%s: %v; aborting post to preserve dedup", row.TaskID, err)
		} else {
			if newCommentID, err := client.PostComment(ctx, row.TaskID, reply, row.CommentID); err != nil {
				log.Printf("[worker] post comment task=%s: %v", row.TaskID, err)
			} else {
				if err := db.OutboxMarkPosted(ctx, hash, newCommentID); err != nil {
					log.Printf("[worker] outbox mark posted task=%s: %v", row.TaskID, err)
				}
			}
		}
	}

	if err := db.MarkInboxProcessed(ctx, row.CommentID); err != nil {
		log.Printf("[worker] mark processed comment=%s: %v", row.CommentID, err)
	}
	if err := db.IncrTurnCount(ctx, row.TaskID); err != nil {
		log.Printf("[worker] incr turn count task=%s: %v", row.TaskID, err)
	}
	return true
}

func newUUID() string {
	if b, err := os.ReadFile("/proc/sys/kernel/random/uuid"); err == nil {
		return strings.TrimSpace(string(b))
	}
	// Fallback: generate RFC 4122 v4 UUID via crypto/rand
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		log.Fatalf("cannot generate UUID: %v", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:])
}

func main() {
	cfg := loadConfig()

	db, err := q.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	client := lark.NewClient(lark.Config{
		AppID: cfg.AppID, AppSecret: cfg.AppSecret, BaseURL: cfg.BaseURL,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	pollTicker := time.NewTicker(cfg.PollInterval)
	defer pollTicker.Stop()

	log.Printf("[supervisor] started (tasklist=%s poll=%s db=%s)", cfg.TasklistID, cfg.PollInterval, cfg.DBPath)

	// Poller runs in its own goroutine so Claude subprocess duration doesn't delay poll ticks.
	go func() {
		pollOnce(ctx, client, db, cfg)
		for {
			select {
			case <-ctx.Done():
				return
			case <-pollTicker.C:
				pollOnce(ctx, client, db, cfg)
			}
		}
	}()

	// Worker loop: drain inbox continuously; sleep 500ms when queue is empty.
	for ctx.Err() == nil {
		if !processOne(ctx, db, client, cfg.MaxTurnsPerSession) {
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	log.Println("[supervisor] shutting down")
}
