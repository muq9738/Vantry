package core

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/pkg/api"
)

func seedSearchThread(t *testing.T, c *Core, id, title string, workspace *api.WorkspaceSpec, archived bool, updated int64, input, output string) {
	t.Helper()
	encoded, err := encodeWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.write(context.Background(), func(tx *txn) error {
		if _, err := tx.exec(`INSERT INTO threads(id, title, created_at, workspace, archived) VALUES (?, ?, ?, ?, ?)`,
			id, title, updated-1, encoded, archived); err != nil {
			return err
		}
		if input == "" && output == "" {
			return nil
		}
		_, err := tx.exec(`INSERT INTO runs(id, thread_id, attempt_id, active_attempt_id, status, input, output, created_at, updated_at, workspace)
			VALUES (?, ?, '', '', ?, ?, ?, ?, ?, ?)`, "run-"+id, id, statusCompleted, input, output, updated-1, updated, encoded)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchThreadsRankingFiltersAndFTSFallback(t *testing.T) {
	ctx := context.Background()
	c := openTestCore(t, emitProvider())
	t.Logf("SQLite FTS5 trigram enabled: %v", c.store.searchFTS)
	seedSearchThread(t, c, "old-title", "Needle title", nil, false, 10, "", "")
	seedSearchThread(t, c, "new-title", "new NEEDLE title", nil, false, 20, "", "")
	seedSearchThread(t, c, "branch", "branch thread", &api.WorkspaceSpec{Branch: "feature/needle", Path: "/projects/app"}, false, 30, "", "")
	seedSearchThread(t, c, "path", "path thread", &api.WorkspaceSpec{Path: "/projects/needle"}, false, 40, "", "")
	seedSearchThread(t, c, "body", "body thread", nil, false, 50, "user needle question", "assistant needle answer")
	seedSearchThread(t, c, "archived", "needle archived", nil, true, 60, "needle hidden", "needle hidden")

	query := api.SearchQuery{Query: "needle"}
	hits, err := c.SearchThreads(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	wantThreads := []string{"new-title", "old-title", "path", "branch", "body", "body"}
	wantFields := []api.SearchField{api.SearchTitle, api.SearchTitle, api.SearchWorkspace, api.SearchBranch, api.SearchInput, api.SearchOutput}
	if len(hits) != len(wantThreads) {
		t.Fatalf("search hits = %+v", hits)
	}
	for i, hit := range hits {
		if hit.ThreadID != wantThreads[i] || hit.Field != wantFields[i] || hit.Snippet == "" {
			t.Fatalf("hit %d = %+v", i, hit)
		}
		if hit.ThreadID == "body" && (hit.RunID != "run-body" || !hit.UpdatedAt.Equal(time.Unix(0, 50).UTC())) {
			t.Fatalf("body identity/activity = %+v", hit)
		}
	}
	fts := c.store.searchFTS
	c.store.searchFTS = false
	scanned, err := c.SearchThreads(ctx, query)
	c.store.searchFTS = fts
	if err != nil || !reflect.DeepEqual(hits, scanned) {
		t.Fatalf("FTS/scanning differ: %+v vs %+v, %v", hits, scanned, err)
	}
	limited, err := c.SearchThreads(ctx, api.SearchQuery{Query: "needle", Limit: 1})
	if err != nil || len(limited) != 1 || limited[0].ThreadID != "new-title" {
		t.Fatalf("limited hits = %+v %v", limited, err)
	}
	archived, err := c.SearchThreads(ctx, api.SearchQuery{Query: "needle", ThreadID: "archived", IncludeArchived: true})
	if err != nil || len(archived) != 3 || archived[0].ThreadID != "archived" {
		t.Fatalf("archived hits = %+v %v", archived, err)
	}
	hidden, err := c.SearchThreads(ctx, api.SearchQuery{Query: "needle", ThreadID: "archived"})
	if err != nil || len(hidden) != 0 {
		t.Fatalf("archived thread leaked: %+v %v", hidden, err)
	}
	missing, err := c.SearchThreads(ctx, api.SearchQuery{Query: "needle", ThreadID: "missing"})
	if err != nil || len(missing) != 0 {
		t.Fatalf("thread filter ignored: %+v %v", missing, err)
	}
}

func TestSearchThreadsChineseSnippetsAndLiteralSymbols(t *testing.T) {
	ctx := context.Background()
	c := openTestCore(t, emitProvider())
	text := strings.Repeat("前文内容", 100) + "中文关键字" + strings.Repeat("后文内容", 100)
	seedSearchThread(t, c, "chinese", "中文会话", nil, false, 10, "用户输入提问", text)
	seedSearchThread(t, c, "symbols", "literal symbols", nil, false, 20, `100%_done " OR 1=1 -- [a:b] C:\work\tree`, "plain 100ZZdone")
	for _, query := range []string{"中文关键字", "关键", "用户输入", "100%_done", `" OR 1=1 --`, "[a:b]", `C:\work\tree`} {
		hits, err := c.SearchThreads(ctx, api.SearchQuery{Query: query})
		if err != nil || len(hits) != 1 {
			t.Fatalf("literal query %q = %+v %v", query, hits, err)
		}
		if !utf8.ValidString(hits[0].Snippet) || !strings.Contains(hits[0].Snippet, query) {
			t.Fatalf("invalid/truncated snippet for %q: %q", query, hits[0].Snippet)
		}
		if utf8.RuneCountInString(hits[0].Snippet) > 162 {
			t.Fatalf("unbounded snippet: %d runes", utf8.RuneCountInString(hits[0].Snippet))
		}
	}
	seedSearchThread(t, c, "escaped-workspace", "escaped paths", &api.WorkspaceSpec{Path: `C:\work\tree`, Branch: `feature/"quoted"`}, false, 30, "", "")
	for _, query := range []string{`C:\work\tree`, `"quoted"`} {
		hits, err := c.SearchThreads(ctx, api.SearchQuery{Query: query, ThreadID: "escaped-workspace"})
		if err != nil || len(hits) != 1 || !strings.Contains(hits[0].Snippet, query) {
			t.Fatalf("escaped workspace query %q = %+v %v", query, hits, err)
		}
	}
	if snippet, ok := searchSnippet("İ"+strings.Repeat("中", 80)+"目标"+strings.Repeat("文", 200), "目标"); !ok || !utf8.ValidString(snippet) || !strings.Contains(snippet, "目标") {
		t.Fatalf("Unicode case-fold changed snippet position: %q, %v", snippet, ok)
	}
}

func TestSearchThreadsIndexesTrackProjectionWrites(t *testing.T) {
	ctx := context.Background()
	c := openTestCore(t, emitProvider())
	seedSearchThread(t, c, "updated", "old search title", nil, false, 10, "old body", "old response")
	if err := c.write(ctx, func(tx *txn) error {
		if _, err := tx.exec(`UPDATE threads SET title = 'replacement title' WHERE id = 'updated'`); err != nil {
			return err
		}
		_, err := tx.exec(`UPDATE runs SET output = 'replacement response' WHERE thread_id = 'updated'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for query, count := range map[string]int{"search title": 0, "old response": 0, "replacement": 2} {
		hits, err := c.SearchThreads(ctx, api.SearchQuery{Query: query})
		if err != nil || len(hits) != count {
			t.Fatalf("updated index query %q = %+v %v", query, hits, err)
		}
	}
	if err := c.write(ctx, func(tx *txn) error {
		_, err := tx.exec(`DELETE FROM runs WHERE thread_id = 'updated'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := c.SearchThreads(ctx, api.SearchQuery{Query: "replacement"})
	if err != nil || len(hits) != 1 || hits[0].Field != api.SearchTitle {
		t.Fatalf("deleted run remains indexed: %+v %v", hits, err)
	}
}

func TestSearchThreadsValidationAndLifecycle(t *testing.T) {
	c := openTestCore(t, emitProvider())
	if hits, err := c.SearchThreads(context.Background(), api.SearchQuery{Query: "  "}); err != nil || hits == nil || len(hits) != 0 {
		t.Fatalf("empty query = %+v %v", hits, err)
	}
	if _, err := c.SearchThreads(context.Background(), api.SearchQuery{Query: "query", Limit: -1}); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("negative limit = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SearchThreads(ctx, api.SearchQuery{Query: "query"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled search = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SearchThreads(context.Background(), api.SearchQuery{Query: "query"}); api.CodeOf(err) != api.NotReady {
		t.Fatalf("search after close = %v", err)
	}
}
