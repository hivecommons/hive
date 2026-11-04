package dashboard

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	checkpointMaxLineRunes   = 4096
	checkpointMaxRegionBytes = 16 << 10
	checkpointMaxBytes       = 512 << 10
)

// Response contract shared by ingest and since(): the reader shows
// checkpointSourceLabel as the feed source and checkpointStage on every region.
const (
	checkpointSourceLabel = "Pushed PR"
	checkpointStage       = "pushed-checkpoint"
)

type checkpointLine struct {
	Op   string `json:"op"`
	Line string `json:"line"`
}

type checkpointRegion struct {
	Seq   uint64           `json:"seq"`
	Repo  string           `json:"repo"`
	PR    int              `json:"pr"`
	PRURL string           `json:"pr_url"`
	Head  string           `json:"head"`
	Path  string           `json:"path"`
	Stage string           `json:"stage"`
	At    time.Time        `json:"at"`
	Lines []checkpointLine `json:"lines"`
}

type checkpointHead struct {
	at time.Time
}

// checkpointSource is a memory-only projection of the existing sentinel
// observer. It never fetches patches, restores Matrix's persisted lines, logs
// content, or broadcasts code. A region is the indivisible unit everywhere.
type checkpointSource struct {
	mu           sync.Mutex
	cfg          config.MatrixConfig
	epoch        string
	seq          uint64
	regions      []checkpointRegion
	lines, bytes int
	heads        map[[sha256.Size]byte]checkpointHead
	rateStart    time.Time
	rateCount    int
}

func newCheckpointSource(cfg config.MatrixConfig) *checkpointSource {
	c := &checkpointSource{}
	c.configure(cfg)
	return c
}

func (c *checkpointSource) configure(cfg config.MatrixConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != "" && c.cfg.Enabled == cfg.Enabled && slices.Equal(c.cfg.Repos, cfg.Repos) && slices.Equal(c.cfg.ExcludePaths, cfg.ExcludePaths) {
		return
	}
	cfg.Repos = append([]string(nil), cfg.Repos...)
	cfg.ExcludePaths = append([]string(nil), cfg.ExcludePaths...)
	c.cfg = cfg
	var nonce [16]byte
	_, _ = rand.Read(nonce[:]) // crypto/rand.Read cannot fail on supported Go versions.
	c.epoch = hex.EncodeToString(nonce[:])
	c.regions = nil
	c.lines, c.bytes, c.rateCount = 0, 0, 0
	c.rateStart = time.Time{}
	c.heads = make(map[[sha256.Size]byte]checkpointHead)
}

func (c *checkpointSource) policyEpoch() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

func (c *checkpointSource) ingest(obs github.SentinelPatchObservation, epoch string, now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.epoch || !c.cfg.Active() || !c.cfg.RepoAllowed(obs.Repo) || obs.Private || !obs.HiveIdentified || obs.Number <= 0 || obs.HeadSHA == "" {
		return 0
	}
	// The sentinel proves public visibility (unknown is Private=true). Only its
	// actual PR/head observations enter; observing the same head cannot replay it.
	key := sha256.Sum256([]byte(strings.ToLower(obs.Repo) + "\x00" + strconv.Itoa(obs.Number) + "\x00" + obs.HeadSHA))
	for k, head := range c.heads {
		if now.Sub(head.at) >= matrixDedupTTL {
			delete(c.heads, k)
		}
	}
	if _, seen := c.heads[key]; seen {
		return 0
	}
	if len(c.heads) >= matrixMaxTrackedPRs {
		var oldest [sha256.Size]byte
		var at time.Time
		for k, head := range c.heads {
			if at.IsZero() || head.at.Before(at) {
				oldest, at = k, head.at
			}
		}
		delete(c.heads, oldest)
	}
	c.heads[key] = checkpointHead{at: now}
	if now.Sub(c.rateStart) >= matrixRateWindow || now.Before(c.rateStart) {
		c.rateStart, c.rateCount = now, 0
	}
	at := obs.At
	if at.IsZero() {
		at = now
	}
	count := 0
	for i, file := range obs.Files {
		if i >= matrixMaxFilesPerObs {
			break
		}
		if matrixPathDenied(file.Path, c.cfg.ExcludePaths) || len(file.Patch) > matrixMaxPatchBytes {
			continue
		}
		for _, lines := range checkpointRegions(file.Patch) {
			size := 0
			safe := len(lines) <= matrixMaxLinesPerHead
			for _, line := range lines {
				size += len(line.Line)
				safe = safe && checkpointSafeLine(line.Line)
			}
			if !safe || size > checkpointMaxRegionBytes {
				continue
			}
			if count+len(lines) > matrixMaxLinesPerHead || c.rateCount+len(lines) > matrixMaxLinesPerRate {
				continue
			}
			c.seq++
			region := checkpointRegion{Seq: c.seq, Repo: obs.Repo, PR: obs.Number, PRURL: fmt.Sprintf("https://github.com/%s/pull/%d", obs.Repo, obs.Number), Head: obs.HeadSHA, Path: file.Path, Stage: checkpointStage, At: at, Lines: lines}
			for len(c.regions) > 0 && (c.lines+len(lines) > matrixBacklogCap || c.bytes+size > checkpointMaxBytes) {
				old := c.regions[0]
				c.lines -= len(old.Lines)
				for _, l := range old.Lines {
					c.bytes -= len(l.Line)
				}
				c.regions[0] = checkpointRegion{}
				c.regions = c.regions[1:]
			}
			c.regions = append(c.regions, region)
			c.lines += len(lines)
			c.bytes += size
			count += len(lines)
			c.rateCount += len(lines)
		}
	}
	return count
}

func checkpointSafeLine(line string) bool {
	if !utf8.ValidString(line) || utf8.RuneCountInString(line) > checkpointMaxLineRunes {
		return false
	}
	for _, r := range line {
		if unicode.IsControl(r) && r != '\t' {
			return false
		}
	}
	return !matrixHasEntropyRun(line) && !matrixKeyValueSecret.MatchString(line) &&
		!strings.Contains(line, "-----BEGIN ") && !strings.Contains(line, "-----END ") &&
		!logscrub.ContainsRedactionMarker(line) && logscrub.ScrubString(line) == line && logscrub.RedactPaneTokens(line) == line
}

var checkpointHunk = regexp.MustCompile(`^@@ -[0-9]+(?:,([0-9]+))? \+[0-9]+(?:,([0-9]+))? @@(?:.*)$`)

// Validate hunk counts before admitting any changed runs. Incomplete or
// malformed patches fail closed, so a truncated hunk never looks complete.
func checkpointRegions(patch string) [][]checkpointLine {
	if patch == "" || strings.ContainsRune(patch, 0) {
		return nil
	}
	rows := strings.Split(patch, "\n")
	if rows[len(rows)-1] == "" {
		rows = rows[:len(rows)-1]
	}
	var out [][]checkpointLine
	var run []checkpointLine
	oldLeft, newLeft := 0, 0
	inHunk := false
	flush := func() {
		if len(run) > 0 {
			out = append(out, run)
			run = nil
		}
	}
	for _, row := range rows {
		if strings.HasPrefix(row, "@@") {
			if inHunk && (oldLeft != 0 || newLeft != 0) {
				return nil
			}
			flush()
			match := checkpointHunk.FindStringSubmatch(row)
			if match == nil {
				return nil
			}
			oldLeft, newLeft = 1, 1
			if match[1] != "" {
				n, e := strconv.Atoi(match[1])
				if e != nil {
					return nil
				}
				oldLeft = n
			}
			if match[2] != "" {
				n, e := strconv.Atoi(match[2])
				if e != nil {
					return nil
				}
				newLeft = n
			}
			inHunk = true
			continue
		}
		if !inHunk {
			continue
		}
		if row == `\ No newline at end of file` {
			continue
		}
		if row == "" {
			return nil
		}
		switch row[0] {
		case '+':
			newLeft--
			run = append(run, checkpointLine{Op: "+", Line: strings.Clone(row[1:])})
		case '-':
			oldLeft--
			run = append(run, checkpointLine{Op: "-", Line: strings.Clone(row[1:])})
		case ' ':
			oldLeft--
			newLeft--
			flush()
		default:
			return nil
		}
		if oldLeft < 0 || newLeft < 0 {
			return nil
		}
	}
	if !inHunk || oldLeft != 0 || newLeft != 0 {
		return nil
	}
	flush()
	return out
}

type checkpointResponse struct {
	Seq           uint64             `json:"seq"`
	Latest        uint64             `json:"latest"`
	Epoch         string             `json:"epoch"`
	Source        string             `json:"source"`
	Stage         string             `json:"stage"`
	Regions       []checkpointRegion `json:"regions"`
	LastIngestAt  *time.Time         `json:"last_ingest_at"`
	NextIngestInS int                `json:"next_ingest_in_s"`
}

// max counts changed lines, but the first eligible region is always returned
// whole even when larger than max. seq advances only through consumed regions.
func (c *checkpointSource) since(seq uint64, limit int) checkpointResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit <= 0 {
		limit = matrixDefaultSinceMax
	}
	if seq > c.seq {
		seq = 0
	}
	out := checkpointResponse{Seq: seq, Latest: c.seq, Epoch: c.epoch, Source: checkpointSourceLabel, Stage: checkpointStage, Regions: []checkpointRegion{}}
	count := 0
	for _, r := range c.regions {
		if r.Seq <= seq {
			continue
		}
		if count > 0 && count+len(r.Lines) > limit {
			break
		}
		out.Regions = append(out.Regions, r)
		count += len(r.Lines)
		out.Seq = r.Seq
	}
	if len(out.Regions) == 0 {
		out.Seq = c.seq
	}
	return out
}
