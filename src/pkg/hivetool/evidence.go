package hivetool

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Citation is one path:line reference found in a terminal summary.
type Citation struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	EndLine  int    `json:"end_line,omitempty"`
	Verified bool   `json:"verified"`
}

// citationRe matches path:line and path:start-end. The range dash accepts
// the hyphen variants models emit (gpt-oss writes U+2011 "149‑168").
var citationRe = regexp.MustCompile(`([A-Za-z0-9_./-]+\.[A-Za-z0-9]+):(\d+)(?:\s*[-–‑]\s*(\d+))?`)

// citationSlack is how far a cited line may sit outside what was served
// (models cite a function's first line while reading from its comment).
const citationSlack = 3

type span struct{ from, to int }

// recordEvidence notes a served location. Callers hold x.mu.
func (x *Executor) recordEvidence(path string, from, to int) {
	if x.evidence == nil {
		x.evidence = map[string][]span{}
	}
	x.evidence[path] = append(x.evidence[path], span{from, to})
}

// Citations extracts path:line references from s and marks each verified
// when this session served that location (a repo.read_file range or a
// repo.search_code hit). It is a deterministic grounding check: it proves the
// model looked where it points, not that its reasoning is right.
func (x *Executor) Citations(s string) []Citation {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.citationsLocked(s)
}

func (x *Executor) citationsLocked(s string) []Citation {
	var out []Citation
	seen := map[string]bool{}
	for _, m := range citationRe.FindAllStringSubmatch(s, -1) {
		p, err := cleanPath(m[1])
		if err != nil || p == "" {
			continue
		}
		line, _ := strconv.Atoi(m[2])
		end := 0
		if m[3] != "" {
			end, _ = strconv.Atoi(m[3])
		}
		key := fmt.Sprintf("%s:%d-%d", p, line, end)
		if seen[key] || line <= 0 {
			continue
		}
		seen[key] = true
		c := Citation{Path: p, Line: line, EndLine: end}
		for _, sp := range x.evidence[p] {
			if line >= sp.from-citationSlack && line <= sp.to+citationSlack {
				c.Verified = true
				break
			}
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Verified && !out[j].Verified })
	return out
}

func (x *Executor) evidenceSummaryLocked() string {
	if len(x.evidence) == 0 {
		return "nothing yet"
	}
	paths := make([]string, 0, len(x.evidence))
	for p := range x.evidence {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) > 12 {
		paths = append(paths[:12], "…")
	}
	return strings.Join(paths, ", ")
}
