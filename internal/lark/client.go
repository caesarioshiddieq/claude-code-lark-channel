package lark

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

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

func (c *Client) ensureToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}
	body, _ := json.Marshal(map[string]string{
		"app_id": c.cfg.AppID, "app_secret": c.cfg.AppSecret,
	})
	resp, err := c.httpClient.Post(
		c.cfg.BaseURL+"/auth/v3/app_access_token/internal",
		"application/json", bytes.NewReader(body),
	)
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

func (c *Client) doGET(path string, params map[string]string) (*http.Response, error) {
	tok, err := c.ensureToken()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, c.cfg.BaseURL+path, nil)
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

func (c *Client) doPOST(path string, body any) (*http.Response, error) {
	tok, err := c.ensureToken()
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return c.httpClient.Do(req)
}

// ListComments fetches one page of comments for taskID. Pass pageToken="" for first page.
func (c *Client) ListComments(taskID, pageToken string) (ListCommentsResult, error) {
	params := map[string]string{"page_size": "50"}
	if pageToken != "" {
		params["page_token"] = pageToken
	}
	resp, err := c.doGET("/task/v2/tasks/"+taskID+"/comments", params)
	if err != nil {
		return ListCommentsResult{}, err
	}
	defer resp.Body.Close()
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
func (c *Client) PostComment(taskID, content, replyToCommentID string) (string, error) {
	body := map[string]any{
		"resource_type": "task",
		"resource_id":   taskID,
		"content":       content,
	}
	if replyToCommentID != "" {
		body["reply_to_comment_id"] = replyToCommentID
	}
	resp, err := c.doPOST("/task/v2/comments", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
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
func (c *Client) ListTasklistTasks(tasklistID string) ([]string, error) {
	pageToken := ""
	var taskIDs []string
	for {
		params := map[string]string{"page_size": "50", "completed": "false"}
		if pageToken != "" {
			params["page_token"] = pageToken
		}
		resp, err := c.doGET("/task/v2/tasklists/"+tasklistID+"/tasks", params)
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
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("list tasklist tasks decode: %w", err)
		}
		resp.Body.Close()
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
