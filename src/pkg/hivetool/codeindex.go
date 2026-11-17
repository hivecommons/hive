package hivetool

import (
	"context"
	"strings"

	"github.com/hivecommons/hive/pkg/codeindex"
)

// CodeIndexSearcher answers repo.search_code from pkg/codeindex, the
// offline index Hive already builds over its registered checkouts. The index
// reflects the checkout it was built from, not an arbitrary ref; ref is
// accepted for the interface and ignored.
type CodeIndexSearcher struct {
	Index func() *codeindex.Index // current snapshot; nil means unavailable
}

// Search implements CodeSearcher. The query is split into exact terms.
func (s CodeIndexSearcher) Search(ctx context.Context, repo, _ string, query, dir string, limit int) ([]Hit, error) {
	idx := (*codeindex.Index)(nil)
	if s.Index != nil {
		idx = s.Index()
	}
	if idx == nil {
		return nil, nil
	}
	terms := strings.Fields(query)
	matches, err := idx.Search(ctx, terms, limit*4)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(dir, "/")
	var out []Hit
	for _, m := range matches {
		if !strings.EqualFold(m.Repo, repo) {
			continue
		}
		if prefix != "" && m.Path != prefix && !strings.HasPrefix(m.Path, prefix+"/") {
			continue
		}
		out = append(out, Hit{Path: m.Path, Line: m.Line, Text: m.Text})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
