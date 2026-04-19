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
	watermark, _, err := db.GetWatermark(taskID)
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
			if err := db.InsertInbox(taskID, c); err != nil {
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
		if err := db.SetWatermark(taskID, latestID); err != nil {
			log.Printf("[poller] set watermark task=%s: %v", taskID, err)
		}
	}
}

func processOne(ctx context.Context, db *q.DB, client *lark.Client, maxTurns int) bool {
	row, found, err := db.NextInboxRow()
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

	sessionUUID, found, err := db.GetSession(row.TaskID)
	if err != nil {
		log.Printf("[worker] get session task=%s: %v", row.TaskID, err)
		return false
	}
	isNew := !found
	if isNew {
		sessionUUID = newUUID()
		if err := db.UpsertSession(row.TaskID, sessionUUID); err != nil {
			log.Printf("[worker] upsert session task=%s: %v", row.TaskID, err)
			return false
		}
	}

	prompt := row.Content
	if turns, _ := db.GetTurnCount(row.TaskID); turns > 0 && turns%maxTurns == 0 {
		prompt = "/compact\n\n" + prompt
	}

	reply, err := worker.SpawnClaude(ctx, sessionUUID, isNew, prompt)
	if err != nil {
		log.Printf("[worker] spawn task=%s comment=%s: %v", row.TaskID, row.CommentID, err)
		if dlqErr := db.MoveToDeadLetter(row.CommentID, row.TaskID, err.Error()); dlqErr != nil {
			log.Printf("[worker] dlq move failed task=%s comment=%s: %v; marking processed", row.TaskID, row.CommentID, dlqErr)
			if mErr := db.MarkInboxProcessed(row.CommentID); mErr != nil {
				log.Printf("[worker] mark processed fallback failed comment=%s: %v", row.CommentID, mErr)
			}
		}
		return true
	}

	hash := worker.ContentHash(row.TaskID, reply)
	existingID, found, outboxErr := db.OutboxCheck(hash)
	if outboxErr != nil {
		log.Printf("[worker] outbox check task=%s: %v", row.TaskID, outboxErr)
	}
	if found && existingID != "" {
		log.Printf("[worker] reply already posted (outbox hit) task=%s", row.TaskID)
	} else {
		db.OutboxInsert(hash, row.TaskID, row.CommentID) //nolint:errcheck
		if newCommentID, err := client.PostComment(ctx, row.TaskID, reply, row.CommentID); err != nil {
			log.Printf("[worker] post comment task=%s: %v", row.TaskID, err)
		} else {
			db.OutboxMarkPosted(hash, newCommentID) //nolint:errcheck
		}
	}

	if err := db.MarkInboxProcessed(row.CommentID); err != nil {
		log.Printf("[worker] mark processed comment=%s: %v", row.CommentID, err)
	}
	if err := db.IncrTurnCount(row.TaskID); err != nil {
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
	pollOnce(ctx, client, db, cfg)

	for {
		select {
		case <-ctx.Done():
			log.Println("[supervisor] shutting down")
			return
		case <-pollTicker.C:
			pollOnce(ctx, client, db, cfg)
		default:
			if !processOne(ctx, db, client, cfg.MaxTurnsPerSession) {
				time.Sleep(500 * time.Millisecond)
			}
		}
	}
}
