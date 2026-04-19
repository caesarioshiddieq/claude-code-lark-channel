package lark_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/lark"
)

func authHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{
		"code": 0, "app_access_token": "tok-test", "expire": 7200,
	})
}

func TestListComments_ReturnsItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/v3/app_access_token/internal" {
			authHandler(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"items": []map[string]any{
					{"comment_id": "c1", "content": "hello", "created_at": int64(1000),
						"creator": map[string]any{"id": "u1", "type": "user"}},
				},
				"has_more": false, "page_token": "",
			},
		})
	}))
	defer srv.Close()

	c := lark.NewClient(lark.Config{AppID: "id", AppSecret: "sec", BaseURL: srv.URL})
	resp, err := c.ListComments("task-abc", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Items[0].CommentID != "c1" {
		t.Fatalf("unexpected items: %+v", resp.Items)
	}
}

func TestPostComment_ReturnsCommentID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/v3/app_access_token/internal" {
			authHandler(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"comment": map[string]any{"comment_id": "new-c99"}},
		})
	}))
	defer srv.Close()

	c := lark.NewClient(lark.Config{AppID: "id", AppSecret: "sec", BaseURL: srv.URL})
	commentID, err := c.PostComment("task-abc", "reply text", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if commentID != "new-c99" {
		t.Fatalf("want new-c99, got %s", commentID)
	}
}
