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

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/budget"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/echo"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/gc"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/lark"
	sqlite "github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
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

func pollOnce(ctx context.Context, client *lark.Client, db *sqlite.DB, cfg config) {
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

func pollTask(ctx context.Context, client *lark.Client, db *sqlite.DB, taskID string, allowList []string) {
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
			if err := db.InsertHumanInbox(ctx, taskID, c); err != nil {
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

func getJitterMinutes() int {
	if v := os.Getenv("NIGHT_JITTER_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 30
}

func resolveSession(ctx context.Context, db *sqlite.DB, taskID string) (uuid string, isNew bool, err error) {
	uuid, exists, err := db.GetSession(ctx, taskID)
	if err != nil {
		return "", false, err
	}
	if !exists {
		uuid = newUUID()
		if err := db.UpsertSession(ctx, taskID, uuid); err != nil {
			return "", false, err
		}
		return uuid, true, nil
	}
	return uuid, false, nil
}

func postDeferredNotice(ctx context.Context, client *lark.Client, taskID, replyTo string) {
	msg := "Autonomous spawn deferred until tonight (day-window active, 05:00–19:00 WIB)."
	if _, err := client.PostComment(ctx, taskID, msg, replyTo); err != nil {
		log.Printf("postDeferredNotice: %v", err)
	}
}

func processOne(ctx context.Context, db *sqlite.DB, client *lark.Client, maxTurns int) bool {
	row, ok, err := db.NextInboxRow(ctx)
	if err != nil {
		log.Printf("processOne: NextInboxRow: %v", err)
		return false
	}
	if !ok {
		return false
	}

	if canSpawn, reason := budget.CanSpawn(ctx, row.Source, time.Now()); !canSpawn {
		log.Printf("processOne: gated %s (%s): %s", row.CommentID, row.Source, reason)
		nextNight := budget.JitteredNightStart(time.Now(), getJitterMinutes())
		if err := db.MarkDeferred(ctx, row.CommentID, nextNight.UnixMilli(), row.Content); err != nil {
			log.Printf("processOne: MarkDeferred: %v", err)
		}
		postDeferredNotice(ctx, client, row.TaskID, row.CommentID)
		return true
	}

	lockFile, err := worker.LockTask(row.TaskID)
	if err != nil {
		log.Printf("processOne: LockTask %s: %v", row.TaskID, err)
		return false
	}
	defer worker.UnlockTask(lockFile)

	sessionUUID, isNew, err := resolveSession(ctx, db, row.TaskID)
	if err != nil {
		log.Printf("processOne: resolveSession %s: %v", row.TaskID, err)
		return false
	}

	switch row.Phase {
	case "answer":
		return dispatchAnswer(ctx, db, client, row, sessionUUID)
	case "compact":
		return dispatchCompact(ctx, db, client, row, sessionUUID)
	default:
		return dispatchNormal(ctx, db, client, row, sessionUUID, isNew, maxTurns)
	}
}

func dispatchNormal(ctx context.Context, db *sqlite.DB, client *lark.Client,
	row sqlite.InboxRow, sessionUUID string, isNew bool, maxTurns int) bool {

	result, err := worker.SpawnClaudeWithUsage(ctx, sessionUUID, isNew, row.Content)
	if usageErr := db.InsertTurnUsage(ctx, sqlite.TurnUsage{
		CommentID: row.CommentID, TaskID: row.TaskID, SessionUUID: sessionUUID, Phase: "normal",
		InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
		CacheReadTokens: result.CacheReadTokens, CacheCreationTokens: result.CacheCreationTokens,
		IsRateLimitError: result.IsRateLimit,
	}); usageErr != nil {
		log.Printf("dispatchNormal: InsertTurnUsage %s: %v", row.CommentID, usageErr)
	}
	if err != nil {
		log.Printf("dispatchNormal: spawn %s: %v", row.CommentID, err)
		if dlqErr := db.MoveToDeadLetter(ctx, row.CommentID, row.TaskID, err.Error()); dlqErr != nil {
			log.Printf("dispatchNormal: MoveToDeadLetter %s: %v", row.CommentID, dlqErr)
		}
		return true
	}

	if outboxErr := db.OutboxInsertPhased(ctx, row.CommentID, row.TaskID, row.CommentID, "normal"); outboxErr != nil {
		log.Printf("dispatchNormal: OutboxInsertPhased %s: %v", row.CommentID, outboxErr)
	}
	if _, err := client.PostComment(ctx, row.TaskID, result.Reply, row.CommentID); err != nil {
		log.Printf("dispatchNormal: PostComment %s: %v", row.CommentID, err)
		return true
	}

	turnCount, _ := db.GetTurnCount(ctx, row.TaskID)
	if turnCount+1 >= maxTurns {
		if phaseErr := db.UpdateInboxPhase(ctx, row.CommentID, "compact", row.Content); phaseErr != nil {
			log.Printf("dispatchNormal: UpdateInboxPhase %s: %v", row.CommentID, phaseErr)
		}
	} else {
		if markErr := db.MarkInboxProcessed(ctx, row.CommentID); markErr != nil {
			log.Printf("dispatchNormal: MarkInboxProcessed %s: %v", row.CommentID, markErr)
		}
		if incrErr := db.IncrTurnCount(ctx, row.TaskID); incrErr != nil {
			log.Printf("dispatchNormal: IncrTurnCount %s: %v", row.TaskID, incrErr)
		}
	}
	return true
}

func dispatchCompact(ctx context.Context, db *sqlite.DB, client *lark.Client,
	row sqlite.InboxRow, sessionUUID string) bool {

	result, err := worker.RunCompactPhase(ctx, sessionUUID)
	if usageErr := db.InsertTurnUsage(ctx, sqlite.TurnUsage{
		CommentID: row.CommentID, TaskID: row.TaskID, SessionUUID: sessionUUID, Phase: "compact",
		InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
		CacheReadTokens: result.CacheReadTokens, CacheCreationTokens: result.CacheCreationTokens,
		IsRateLimitError: result.IsRateLimit,
	}); usageErr != nil {
		log.Printf("dispatchCompact: InsertTurnUsage %s: %v", row.CommentID, usageErr)
	}
	if err != nil {
		log.Printf("dispatchCompact: spawn error %s — resetting to normal: %v", row.CommentID, err)
		if resetErr := db.ResetInboxPhase(ctx, row.CommentID); resetErr != nil {
			log.Printf("dispatchCompact: ResetInboxPhase %s: %v", row.CommentID, resetErr)
		}
		return true
	}

	if outboxErr := db.OutboxInsertPhased(ctx, row.CommentID, row.TaskID, row.CommentID, "compact"); outboxErr != nil {
		log.Printf("dispatchCompact: OutboxInsertPhased %s: %v", row.CommentID, outboxErr)
	}
	if _, err := client.PostComment(ctx, row.TaskID,
		"Context summarized (turn cap reached).", row.CommentID); err != nil {
		log.Printf("dispatchCompact: PostComment %s: %v", row.CommentID, err)
	}
	if phaseErr := db.UpdateInboxPhase(ctx, row.CommentID, "answer", row.OriginalContent); phaseErr != nil {
		log.Printf("dispatchCompact: UpdateInboxPhase %s: %v", row.CommentID, phaseErr)
	}
	return true
}

func dispatchAnswer(ctx context.Context, db *sqlite.DB, client *lark.Client,
	row sqlite.InboxRow, sessionUUID string) bool {

	result, err := worker.RunAnswerPhase(ctx, sessionUUID, row.OriginalContent)
	if usageErr := db.InsertTurnUsage(ctx, sqlite.TurnUsage{
		CommentID: row.CommentID, TaskID: row.TaskID, SessionUUID: sessionUUID, Phase: "answer",
		InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
		CacheReadTokens: result.CacheReadTokens, CacheCreationTokens: result.CacheCreationTokens,
		IsRateLimitError: result.IsRateLimit,
	}); usageErr != nil {
		log.Printf("dispatchAnswer: InsertTurnUsage %s: %v", row.CommentID, usageErr)
	}
	if err != nil {
		log.Printf("dispatchAnswer: spawn %s: %v", row.CommentID, err)
		if dlqErr := db.MoveToDeadLetter(ctx, row.CommentID, row.TaskID, err.Error()); dlqErr != nil {
			log.Printf("dispatchAnswer: MoveToDeadLetter %s: %v", row.CommentID, dlqErr)
		}
		return true
	}

	if outboxErr := db.OutboxInsertPhased(ctx, row.CommentID, row.TaskID, row.CommentID, "answer"); outboxErr != nil {
		log.Printf("dispatchAnswer: OutboxInsertPhased %s: %v", row.CommentID, outboxErr)
	}
	if _, err := client.PostComment(ctx, row.TaskID, result.Reply, row.CommentID); err != nil {
		log.Printf("dispatchAnswer: PostComment %s: %v", row.CommentID, err)
		return true
	}

	if resetErr := db.ResetTurnCount(ctx, row.TaskID); resetErr != nil {
		log.Printf("dispatchAnswer: ResetTurnCount %s: %v", row.TaskID, resetErr)
	}
	if markErr := db.MarkInboxProcessed(ctx, row.CommentID); markErr != nil {
		log.Printf("dispatchAnswer: MarkInboxProcessed %s: %v", row.CommentID, markErr)
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

	db, err := sqlite.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	client := lark.NewClient(lark.Config{
		AppID: cfg.AppID, AppSecret: cfg.AppSecret, BaseURL: cfg.BaseURL,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := budget.ReconcileStaleDeferrals(ctx, db, time.Now()); err != nil {
		log.Printf("boot reconciler: %v", err)
	}
	budget.RunWatchdog(ctx, db)
	gc.RunUsageGC(ctx, db)

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
