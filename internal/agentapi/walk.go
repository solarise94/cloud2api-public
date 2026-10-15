package agentapi

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

// walkBudget bounds tree walks, search walks and dry-run accounting.
const (
	walkDefaultMaxEntries = 2000
	walkHardMaxEntries    = 10000
	searchMaxEntries      = 5000
	searchBudget          = 10 * time.Second
	treeBudget            = 30 * time.Second
)

// walkStats is the outcome of a bounded recursive walk of one path.
type walkStats struct {
	Files     int64
	Dirs      int64
	Bytes     int64
	Items     []string // visible paths (the root itself included), first N
	Truncated bool
}

// walkPath stats logical (a path on c.drive inside c.root) and, for
// directories, walks descendants breadth-first collecting up to maxEntries
// items. It never returns an error for a missing path — the caller checked
// existence beforehand.
func (s *Server) walkPath(ctx context.Context, c *call, logical string, maxEntries int, deadline time.Time) (walkStats, *APIError) {
	if maxEntries <= 0 {
		maxEntries = walkDefaultMaxEntries
	}
	if maxEntries > walkHardMaxEntries {
		maxEntries = walkHardMaxEntries
	}
	_, internal, visible, e := s.resolve(c, logical)
	if e != nil {
		return walkStats{}, e
	}
	stats := walkStats{Items: []string{}}
	if err := s.collectInto(ctx, &stats, c.drive, internal, visible, maxEntries, deadline); err != nil {
		return stats, fsError(err)
	}
	return stats, nil
}

// collectInto adds dir itself (as visible) and its subtree to stats.
func (s *Server) collectInto(ctx context.Context, stats *walkStats, drive, internal, visible string, maxEntries int, deadline time.Time) error {
	info, err := s.fsGet(ctx, internal)
	if err != nil {
		return err
	}
	if !info.IsDir {
		stats.Files++
		stats.Bytes += info.Size
		if len(stats.Items) < maxEntries {
			stats.Items = append(stats.Items, visible)
		} else {
			stats.Truncated = true
		}
		return nil
	}
	stats.Dirs++
	if len(stats.Items) < maxEntries {
		stats.Items = append(stats.Items, visible)
	} else {
		stats.Truncated = true
	}
	type queueItem struct{ internal, visible string }
	queue := []queueItem{{internal, visible}}
	for len(queue) > 0 {
		if ctxErr := ctx.Err(); ctxErr != nil {
			stats.Truncated = true
			return nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			stats.Truncated = true
			return nil
		}
		cur := queue[0]
		queue = queue[1:]
		page := 1
		for {
			entries, total, err := s.fsList(ctx, cur.internal, page, 1000)
			if err != nil {
				return err
			}
			for _, en := range entries {
				if (en.Name == ".c2a-trash" || en.Name == relayDirName) && cur.visible == "/" {
					continue
				}
				childVisible := path.Join(cur.visible, en.Name)
				childInternal := path.Join(cur.internal, en.Name)
				if en.IsDir {
					stats.Dirs++
					if len(stats.Items) < maxEntries {
						stats.Items = append(stats.Items, childVisible)
					} else {
						stats.Truncated = true
					}
					queue = append(queue, queueItem{childInternal, childVisible})
				} else {
					stats.Files++
					stats.Bytes += en.Size
					if len(stats.Items) < maxEntries {
						stats.Items = append(stats.Items, childVisible)
					} else {
						stats.Truncated = true
					}
				}
			}
			if page*1000 >= total || len(entries) == 0 {
				break
			}
			page++
		}
	}
	return nil
}

// ---- name similarity suggestions (§4.10.6) ----

// suggestionsFor lists the parent of missing and returns up to 3 sibling
// paths by name similarity (case-insensitive Levenshtein). The parent path is
// the agent-visible form.
func (s *Server) suggestionsFor(ctx context.Context, c *call, visible string) []string {
	parent, _ := parentOf(visible)
	if parent == "" {
		parent = "/"
	}
	_, internal, _, e := s.resolve(c, parent)
	if e != nil {
		return nil
	}
	entries, _, err := s.fsList(ctx, internal, 1, 500)
	if err != nil {
		return nil
	}
	return suggestNames(parent, path.Base(visible), entries)
}

// suggestNames picks up to maxSuggestions similar names from entries.
func suggestNames(parent, want string, entries []fsFileInfo) []string {
	const maxSuggestions = 3
	type scored struct {
		name string
		dist int
	}
	lowerWant := strings.ToLower(want)
	var cands []scored
	for _, en := range entries {
		if en.Name == ".c2a-trash" || en.Name == relayDirName {
			continue
		}
		lower := strings.ToLower(en.Name)
		if lower == lowerWant {
			cands = append([]scored{{en.Name, -1}}, cands...) // exact (case differs) first
			continue
		}
		d := levenshtein(lowerWant, lower)
		budget := len(lowerWant)/3 + 2
		if d <= budget {
			cands = append(cands, scored{en.Name, d})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].dist != cands[j].dist {
			return cands[i].dist < cands[j].dist
		}
		return cands[i].name < cands[j].name
	})
	out := make([]string, 0, maxSuggestions)
	for _, cd := range cands {
		if len(out) >= maxSuggestions {
			break
		}
		out = append(out, path.Join(parent, cd.name))
	}
	return out
}

// levenshtein computes the edit distance of two strings.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := 0; j <= len(rb); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// ---- /v1/tree and /v1/search (§4.10.5) ----

func (s *Server) handleTree(c *call) {
	q := c.r.URL.Query()
	logical := q.Get("path")
	if logical == "" {
		logical = "/"
	}
	depth, e := parseInt(q.Get, "depth", 3, 10)
	if e != nil {
		c.fail(e)
		return
	}
	maxEntries, e := parseInt(q.Get, "max_entries", walkDefaultMaxEntries, walkHardMaxEntries)
	if e != nil {
		c.fail(e)
		return
	}
	_, internal, visible, e := s.resolve(c, logical)
	if e != nil {
		c.fail(e)
		return
	}
	c.setPath(visible)
	ctx, cancel := context.WithTimeout(c.r.Context(), treeBudget)
	defer cancel()

	info, err := s.fsGet(ctx, internal)
	if err != nil {
		if isNotFoundError(err) {
			ae := errPathNotFound(visible)
			ae.Suggestions = s.suggestionsFor(ctx, c, visible)
			c.fail(ae)
			return
		}
		c.fail(fsError(err))
		return
	}
	type node struct {
		Internal string
		Visible  string
		Depth    int
	}
	items := []map[string]any{}
	truncated := false
	count := 0
	if !info.IsDir {
		items = append(items, map[string]any{"path": visible, "is_dir": false, "size": info.Size, "modified": info.Modified})
		c.ok(map[string]any{"path": visible, "depth": 0, "items": items, "truncated": false})
		return
	}
	queue := []node{{internal, visible, 0}}
	for len(queue) > 0 && !truncated {
		if ctx.Err() != nil {
			truncated = true
			break
		}
		cur := queue[0]
		queue = queue[1:]
		entries, _, err := s.fsList(ctx, cur.Internal, 1, 1000)
		if err != nil {
			c.fail(fsError(err))
			return
		}
		for _, en := range entries {
			if (en.Name == ".c2a-trash" || en.Name == relayDirName) && cur.Visible == "/" {
				continue
			}
			if count >= maxEntries {
				truncated = true
				break
			}
			count++
			childVisible := path.Join(cur.Visible, en.Name)
			items = append(items, map[string]any{
				"path":     childVisible,
				"is_dir":   en.IsDir,
				"size":     en.Size,
				"modified": en.Modified,
				"depth":    cur.Depth + 1,
			})
			if en.IsDir && cur.Depth+1 < depth {
				queue = append(queue, node{path.Join(cur.Internal, en.Name), childVisible, cur.Depth + 1})
			}
		}
	}
	c.ok(map[string]any{"path": visible, "depth": depth, "items": items, "truncated": truncated})
}

// searchState caches whether OpenList's search index is enabled ("search not
// available" answered live once, then remembered for a while).
type searchState struct {
	at        time.Time
	available bool
}

func (s *Server) handleSearch(c *call) {
	q := c.r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		c.fail(errInvalidRequest("query parameter `q` is required"))
		return
	}
	logical := q.Get("path")
	if logical == "" {
		logical = "/"
	}
	_, internal, visible, e := s.resolve(c, logical)
	if e != nil {
		c.fail(e)
		return
	}
	c.setPath(visible)
	ctx, cancel := context.WithTimeout(c.r.Context(), searchBudget)
	defer cancel()

	// Prefer OpenList's search index when it exists (probed live).
	if s.openlistSearchOK() {
		data, err := s.fsCall(ctx, http.MethodPost, "/api/fs/search", map[string]any{
			"path": internal, "keywords": query, "page": 1, "per_page": 200, "scope": 0,
		})
		if err == nil {
			var doc struct {
				Content []struct {
					Name     string `json:"name"`
					Parent   string `json:"parent"`
					IsDir    bool   `json:"is_dir"`
					Size     int64  `json:"size"`
					Modified string `json:"modified"`
				} `json:"content"`
				Total int `json:"total"`
			}
			if json.Unmarshal(data, &doc) == nil {
				items := []map[string]any{}
				for _, en := range doc.Content {
					// Results carry the parent dir plus the entry name.
					full := path.Join(en.Parent, en.Name)
					items = append(items, map[string]any{
						"path":     full,
						"is_dir":   en.IsDir,
						"size":     en.Size,
						"modified": en.Modified,
					})
				}
				c.ok(map[string]any{"items": items, "truncated": doc.Total > len(items), "engine": "openlist"})
				return
			}
		} else if !isSearchUnavailable(err) {
			c.fail(fsError(err))
			return
		} else {
			s.markSearchUnavailable()
		}
	}

	// Fallback: bounded walk, case-insensitive substring match.
	res := s.boundedSearch(ctx, c, internal, visible, strings.ToLower(query))
	c.ok(map[string]any{"items": res.items, "truncated": res.truncated, "engine": "walk"})
}

type searchResult struct {
	items     []map[string]any
	truncated bool
}

func (s *Server) boundedSearch(ctx context.Context, c *call, internal, visible, lowerQuery string) searchResult {
	out := searchResult{items: []map[string]any{}}
	type queueItem struct{ internal, visible string }
	queue := []queueItem{{internal, visible}}
	visited := 0
	for len(queue) > 0 {
		if ctx.Err() != nil {
			out.truncated = true
			return out
		}
		cur := queue[0]
		queue = queue[1:]
		page := 1
		for {
			entries, total, err := s.fsList(ctx, cur.internal, page, 1000)
			if err != nil {
				return out // partial results; upstream errors surface as empty
			}
			for _, en := range entries {
				if (en.Name == ".c2a-trash" || en.Name == relayDirName) && cur.visible == "/" {
					continue
				}
				visited++
				if visited > searchMaxEntries {
					out.truncated = true
					return out
				}
				childVisible := path.Join(cur.visible, en.Name)
				if strings.Contains(strings.ToLower(en.Name), lowerQuery) {
					out.items = append(out.items, map[string]any{
						"path":     childVisible,
						"is_dir":   en.IsDir,
						"size":     en.Size,
						"modified": en.Modified,
					})
				}
				if en.IsDir {
					queue = append(queue, queueItem{path.Join(cur.internal, en.Name), childVisible})
				}
			}
			if page*1000 >= total || len(entries) == 0 {
				break
			}
			page++
		}
	}
	return out
}

// openlistSearchOK reports whether the OpenList search index is usable,
// probing at most once per 5 minutes.
func (s *Server) openlistSearchOK() bool {
	s.searchMu.Lock()
	defer s.searchMu.Unlock()
	if s.searchProbe.at.IsZero() || time.Since(s.searchProbe.at) > 5*time.Minute {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := s.fsCall(ctx, http.MethodPost, "/api/fs/search", map[string]any{
			"path": "/", "keywords": "c2a-probe", "page": 1, "per_page": 1, "scope": 0,
		})
		s.searchProbe.at = time.Now()
		s.searchProbe.available = err == nil
	}
	return s.searchProbe.available
}

func (s *Server) markSearchUnavailable() {
	s.searchMu.Lock()
	s.searchProbe.at = time.Now()
	s.searchProbe.available = false
	s.searchMu.Unlock()
}

func isSearchUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "search not available")
}
