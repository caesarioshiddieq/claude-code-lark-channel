package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// validID guards against path-traversal / injection via user-supplied IDs.
var validID = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)

func validateID(id, field string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid %s: %q", field, id)
	}
	return nil
}

type Config struct {
	AppID     string
	AppSecret string
	BaseURL   string
}

type Creator struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "app" = bot, "user" = human
}

type Comment struct {
	CommentID string  `json:"comment_id"`
	Creator   Creator `json:"creator"`
	Content   string  `json:"content"`
	CreatedAt int64   `json:"created_at"` // milliseconds
}

type ListCommentsResult struct {
	Items     []Comment
	HasMore   bool
	PageToken string
}

type Client struct {
	cfg        Config
	httpClient *http.Client
	mu         sync.Mutex
	token      string
	tokenExp   time.Time
}

func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg, httpClient: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}
	// Fix 1: handle json.Marshal error instead of ignoring it.
	body, err := json.Marshal(map[string]string{
		"app_id": c.cfg.AppID, "app_secret": c.cfg.AppSecret,
	})
	if err != nil {
		return "", fmt.Errorf("lark auth marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/auth/v3/app_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("lark auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("lark auth: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Code           int    `json:"code"`
		Msg            string `json:"msg"`
		AppAccessToken string `json:"app_access_token"`
		Expire         int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("lark auth decode: %w", err)
	}
	if out.Code != 0 {
		return "", fmt.Errorf("lark auth: %s", out.Msg)
	}
	c.token = out.AppAccessToken
	c.tokenExp = time.Now().Add(time.Duration(out.Expire)*time.Second - 5*time.Minute)
	return c.token, nil
}

func (c *Client) doGET(ctx context.Context, path string, params map[string]string) (*http.Response, error) {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	q := req.URL.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()
	return c.httpClient.Do(req)
}

func (c *Client) doPOST(ctx context.Context, path string, body any) (*http.Response, error) {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return c.httpClient.Do(req)
}

// ListComments fetches one page of comments for taskID. Pass pageToken="" for first page.
func (c *Client) ListComments(ctx context.Context, taskID, pageToken string) (ListCommentsResult, error) {
	// Fix 2: validate taskID before URL interpolation.
	if err := validateID(taskID, "taskID"); err != nil {
		return ListCommentsResult{}, err
	}
	params := map[string]string{"page_size": "50"}
	if pageToken != "" {
		params["page_token"] = pageToken
	}
	resp, err := c.doGET(ctx, "/task/v2/tasks/"+taskID+"/comments", params)
	if err != nil {
		return ListCommentsResult{}, err
	}
	defer resp.Body.Close()
	// Fix 4: check HTTP status before decoding.
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return ListCommentsResult{}, fmt.Errorf("lark API HTTP %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Items     []Comment `json:"items"`
			HasMore   bool      `json:"has_more"`
			PageToken string    `json:"page_token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ListCommentsResult{}, fmt.Errorf("list comments decode: %w", err)
	}
	if out.Code != 0 {
		return ListCommentsResult{}, fmt.Errorf("list comments: %s", out.Msg)
	}
	return ListCommentsResult{Items: out.Data.Items, HasMore: out.Data.HasMore, PageToken: out.Data.PageToken}, nil
}

// PostComment posts content as a reply to replyToCommentID on taskID.
// Pass replyToCommentID="" for a top-level comment. Returns the new comment_id.
func (c *Client) PostComment(ctx context.Context, taskID, content, replyToCommentID string) (string, error) {
	body := map[string]any{
		"resource_type": "task",
		"resource_id":   taskID,
		"content":       content,
	}
	if replyToCommentID != "" {
		body["reply_to_comment_id"] = replyToCommentID
	}
	resp, err := c.doPOST(ctx, "/task/v2/comments", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// Fix 4: check HTTP status before decoding.
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("lark API HTTP %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Comment Comment `json:"comment"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("post comment decode: %w", err)
	}
	if out.Code != 0 {
		return "", fmt.Errorf("post comment: %s", out.Msg)
	}
	return out.Data.Comment.CommentID, nil
}

// ListTasklistTasks returns GUIDs of all non-completed tasks in a tasklist.
// ⚠ Verify "guid" field name against Lark Task v2 API docs before deploying.
func (c *Client) ListTasklistTasks(ctx context.Context, tasklistID string) ([]string, error) {
	// Fix 2: validate tasklistID before URL interpolation.
	if err := validateID(tasklistID, "tasklistID"); err != nil {
		return nil, err
	}
	pageToken := ""
	var taskIDs []string
	for {
		params := map[string]string{"page_size": "50", "completed": "false"}
		if pageToken != "" {
			params["page_token"] = pageToken
		}
		resp, err := c.doGET(ctx, "/task/v2/tasklists/"+tasklistID+"/tasks", params)
		if err != nil {
			return nil, err
		}
		var out struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				Items []struct {
					GUID string `json:"guid"`
				} `json:"items"`
				HasMore   bool   `json:"has_more"`
				PageToken string `json:"page_token"`
			} `json:"data"`
		}
		// Fix 3: use inline closure so defer fires before next iteration.
		// Fix 4: check HTTP status before decoding.
		decErr := func() error {
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("lark API HTTP %d: %s", resp.StatusCode, b)
			}
			return json.NewDecoder(resp.Body).Decode(&out)
		}()
		if decErr != nil {
			return nil, fmt.Errorf("list tasklist tasks decode: %w", decErr)
		}
		if out.Code != 0 {
			return nil, fmt.Errorf("list tasklist tasks: %s", out.Msg)
		}
		for _, item := range out.Data.Items {
			if item.GUID != "" {
				taskIDs = append(taskIDs, item.GUID)
			}
		}
		if !out.Data.HasMore {
			break
		}
		pageToken = out.Data.PageToken
	}
	return taskIDs, nil
}
