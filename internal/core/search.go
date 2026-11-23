package core

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/muq9738/Vantry/pkg/api"
)

// initSearch uses FTS5 trigram indexes for literal substring candidates, with
// triggers in the projection transaction. Existing data is indexed only when
// an index is first created. Builds without FTS5/trigram use projection scans.
func initSearch(ctx context.Context, db *sql.DB) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("core: begin search initialization: %w", err)
	}
	defer tx.Rollback()
	for _, spec := range []struct{ table, index, columns string }{
		{"threads", "thread_search", "title, workspace"},
		{"runs", "run_search", "input, output, workspace"},
	} {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name = ?)`, spec.index).Scan(&exists); err != nil {
			return false, fmt.Errorf("core: inspect search index: %w", err)
		}
		statement := fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING fts5(%s,
			content='%s', content_rowid='rowid', tokenize='trigram')`, spec.index, spec.columns, spec.table)
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			message := strings.ToLower(err.Error())
			if strings.Contains(message, "no such module: fts5") || strings.Contains(message, "no such tokenizer: trigram") {
				return false, nil
			}
			return false, fmt.Errorf("core: initialize search index: %w", err)
		}
		var oldColumns, newColumns []string
		for _, column := range strings.Split(spec.columns, ", ") {
			oldColumns = append(oldColumns, "old."+column)
			newColumns = append(newColumns, "new."+column)
		}
		insert := fmt.Sprintf(`INSERT INTO %s(rowid, %s) VALUES (new.rowid, %s);`,
			spec.index, spec.columns, strings.Join(newColumns, ", "))
		remove := fmt.Sprintf(`INSERT INTO %s(%s, rowid, %s) VALUES ('delete', old.rowid, %s);`,
			spec.index, spec.index, spec.columns, strings.Join(oldColumns, ", "))
		for _, trigger := range []struct{ suffix, event, body string }{
			{"insert", "INSERT", insert},
			{"delete", "DELETE", remove},
			{"update", "UPDATE OF " + spec.columns, remove + insert},
		} {
			statement := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_%s AFTER %s ON %s BEGIN %s END`,
				spec.index, trigger.suffix, trigger.event, spec.table, trigger.body)
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return false, fmt.Errorf("core: initialize search trigger: %w", err)
			}
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(%s) VALUES ('rebuild')`, spec.index, spec.index)); err != nil {
				return false, fmt.Errorf("core: index existing search content: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("core: commit search initialization: %w", err)
	}
	return true, nil
}

// SearchThreads reads committed projections, so results survive event pruning.
// Each matching field produces a hit; title > branch/workspace > message body,
// with the most recently active thread first within each weight.
func (c *Core) SearchThreads(ctx context.Context, query api.SearchQuery) ([]api.SearchHit, error) {
	c.lifeMu.RLock()
	defer c.lifeMu.RUnlock()
	if c.state.Load() == stateStopped {
		return nil, errorf(api.NotReady, "core is stopped")
	}
	if query.Limit < 0 {
		return nil, errorf(api.InvalidCommand, "search limit must not be negative")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	term := strings.TrimSpace(query.Query)
	hits := make([]api.SearchHit, 0)
	if term == "" {
		return hits, nil
	}
	limit := query.Limit
	if limit == 0 {
		limit = 50
	}

	filter := ""
	var filterArgs []any
	if !query.IncludeArchived {
		filter += " AND t.archived = 0"
	}
	if query.ThreadID != "" {
		filter += " AND t.id = ?"
		filterArgs = append(filterArgs, query.ThreadID)
	}
	// Search decoded workspace fields: JSON escaping must not hide quotes or
	// backslashes that are part of a real branch name or filesystem path.
	workspaceMatch := func(alias string) string {
		return `(json_extract(NULLIF(` + alias + `.workspace, ''), '$.branch') LIKE ? ESCAPE '\'
			OR json_extract(NULLIF(` + alias + `.workspace, ''), '$.path') LIKE ? ESCAPE '\'
			OR json_extract(NULLIF(` + alias + `.workspace, ''), '$.repository') LIKE ? ESCAPE '\')`
	}
	threadMatch := `(t.title LIKE ? ESCAPE '\' OR ` + workspaceMatch("t") + `)`
	runMatch := `(r.input LIKE ? ESCAPE '\' OR r.output LIKE ? ESCAPE '\' OR ` + workspaceMatch("r") + `)`
	pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(term) + "%"
	threadArgs := []any{pattern, pattern, pattern, pattern}
	runArgs := []any{pattern, pattern, pattern, pattern, pattern}
	if c.store.searchFTS && canSearchFTS(term) {
		threadMatch = `t.rowid IN (SELECT rowid FROM thread_search WHERE thread_search MATCH ?)`
		runMatch = `r.rowid IN (SELECT rowid FROM run_search WHERE run_search MATCH ?)`
		// MATCH receives a quoted literal, never FTS syntax supplied by callers.
		literal := `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
		threadArgs, runArgs = []any{literal}, []any{literal}
	}
	args := append(threadArgs, filterArgs...)
	args = append(args, runArgs...)
	args = append(args, filterArgs...)
	rows, err := c.store.db.QueryContext(ctx, `
		WITH candidates AS (
			SELECT t.id AS thread_id, '' AS run_id, t.title, t.workspace, '' AS input, '' AS output
			FROM threads t WHERE `+threadMatch+filter+`
			UNION ALL
			SELECT t.id, r.id, t.title, r.workspace, r.input, r.output
			FROM runs r JOIN threads t ON t.id = r.thread_id WHERE `+runMatch+filter+`
		)
		SELECT c.thread_id, c.run_id, c.title, c.workspace, c.input, c.output,
			MAX(t.created_at, COALESCE((SELECT MAX(updated_at) FROM runs WHERE thread_id = t.id), 0))
		FROM candidates c JOIN threads t ON t.id = c.thread_id`, args...)
	if err != nil {
		return nil, wrapInternal("search threads", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hit api.SearchHit
		var workspaceJSON, input, output string
		var updated int64
		if err := rows.Scan(&hit.ThreadID, &hit.RunID, &hit.Title, &workspaceJSON, &input, &output, &updated); err != nil {
			return nil, wrapInternal("scan search hit", err)
		}
		hit.UpdatedAt = fromNanos(updated)
		add := func(field api.SearchField, text string, score int) {
			if snippet, ok := searchSnippet(text, term); ok {
				candidate := hit
				candidate.Field, candidate.Snippet, candidate.Score = field, snippet, score
				hits = append(hits, candidate)
			}
		}
		if hit.RunID == "" {
			add(api.SearchTitle, hit.Title, 300)
		} else {
			add(api.SearchInput, input, 100)
			add(api.SearchOutput, output, 100)
		}
		if spec := decodeWorkspace(workspaceJSON); spec != nil {
			add(api.SearchBranch, spec.Branch, 200)
			add(api.SearchWorkspace, spec.Path, 200)
			if spec.Repository != spec.Path {
				add(api.SearchWorkspace, spec.Repository, 200)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, wrapInternal("read search hits", err)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		if a.ThreadID != b.ThreadID {
			return a.ThreadID < b.ThreadID
		}
		if a.RunID != b.RunID {
			return a.RunID < b.RunID
		}
		return a.Field < b.Field
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func canSearchFTS(term string) bool {
	if utf8.RuneCountInString(term) < 3 {
		return false
	}
	for _, r := range term {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// searchSnippet counts runes on both sides of the match. Lowercasing can change
// byte lengths, so even the match position must be converted to a rune offset.
func searchSnippet(text, term string) (string, bool) {
	folded := strings.ToLower(text)
	index := strings.Index(folded, strings.ToLower(term))
	if index < 0 {
		return "", false
	}
	runes := []rune(text)
	match := utf8.RuneCountInString(folded[:index])
	start := max(0, match-40)
	end := min(len(runes), max(start+160, match+utf8.RuneCountInString(term)))
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(runes) {
		suffix = "…"
	}
	return prefix + string(runes[start:end]) + suffix, true
}
