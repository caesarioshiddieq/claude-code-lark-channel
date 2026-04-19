package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
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
	return config{
		AppID:              mustEnv("LARK_APP_ID"),
		AppSecret:          mustEnv("LARK_APP_SECRET"),
		BaseURL:            mustEnv("LARK_BASE_URL"),
		TasklistID:         mustEnv("LARK_TASKLIST_ID"),
		AllowList:          allowList,
		DBPath:             dbPath,
		PollInterval:       pollInterval,
		MaxTurnsPerSession: 50,
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
	tasks, err := client.ListTasklistTasks(cfg.TasklistID)
	if err != nil {
		log.Printf("[poller] list tasks: %v", err)
		return
	}
	for _, taskID := range tasks {
		if ctx.Err() != nil {
			return
		}
		pollTask(client, db, taskID, cfg.AllowList)
	}
}

func pollTask(client *lark.Client, db *q.DB, taskID string, allowList []string) {
	watermark, _, _ := db.GetWatermark(taskID)
	pageToken := ""
	var latestID string
	for {
		resp, err := client.ListComments(taskID, pageToken)
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

func processOne(db *q.DB, client *lark.Client, maxTurns int) bool {
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

	reply, err := worker.SpawnClaude(sessionUUID, isNew, prompt)
	if err != nil {
		log.Printf("[worker] spawn task=%s comment=%s: %v", row.TaskID, row.CommentID, err)
		db.MoveToDeadLetter(row.CommentID, row.TaskID, err.Error()) //nolint:errcheck
		return true
	}

	hash := worker.ContentHash(row.TaskID, reply)
	if existingID, posted, _ := db.OutboxCheck(hash); posted && existingID != "" {
		log.Printf("[worker] reply already posted (outbox hit) task=%s", row.TaskID)
	} else {
		db.OutboxInsert(hash, row.TaskID, row.CommentID) //nolint:errcheck
		if newCommentID, err := client.PostComment(row.TaskID, reply, row.CommentID); err != nil {
			log.Printf("[worker] post comment task=%s: %v", row.TaskID, err)
		} else {
			db.OutboxMarkPosted(hash, newCommentID) //nolint:errcheck
		}
	}

	db.MarkInboxProcessed(row.CommentID) //nolint:errcheck
	db.IncrTurnCount(row.TaskID)         //nolint:errcheck
	return true
}

func newUUID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/uuid")
	if err != nil {
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	return strings.TrimSpace(string(b))
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
			if !processOne(db, client, cfg.MaxTurnsPerSession) {
				time.Sleep(500 * time.Millisecond)
			}
		}
	}
}
