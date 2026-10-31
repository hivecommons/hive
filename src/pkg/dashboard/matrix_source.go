package dashboard

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Matrix line source bounds (#15934). Every buffer is fixed so a burst of
// pushes can cost memory and CPU only up to these limits.
const (
	matrixIngestCap       = 64
	matrixMaxLineRunes    = 160
	matrixMaxLinesPerHead = 200
	matrixBacklogCap      = 2000
	matrixMaxTrackedPRs   = 500
	matrixDedupTTL        = 24 * time.Hour
	matrixRateWindow      = time.Minute
	matrixMaxLinesPerRate = 600
	matrixMaxFilesPerObs  = 300
	matrixMaxPatchBytes   = 512 << 10
	matrixDefaultSinceMax = 200
)

// matrixMaxDedupHashes caps dedup hashes across every tracked PR; a var so
// tests can exercise the bound without 100k lines.
var matrixMaxDedupHashes = 100000

// matrixLine is one accepted, scrub-clean added line.
type matrixLine struct {
	Seq  uint64    `json:"seq"`
	Repo string    `json:"repo"`
	PR   int       `json:"pr"`
	Head string    `json:"head"`
	Path string    `json:"path"`
	Lang string    `json:"lang,omitempty"`
	Line string    `json:"line"`
	At   time.Time `json:"at"`
}

type matrixPRState struct {
	head      string
	headCount int
	hashes    map[[sha256.Size]byte]time.Time
	touched   time.Time
}

// matrixSource turns sentinel patch observations into a bounded, scrubbed,
// de-duplicated in-memory backlog. Observe never blocks; one goroutine does
// all the parsing. Nothing is written to disk.
type matrixSource struct {
	cfg     atomic.Pointer[config.MatrixConfig]
	in      chan github.SentinelPatchObservation
	dropped atomic.Uint64
	logger  *slog.Logger
	now     func() time.Time

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}

	mu          sync.Mutex
	ring        []matrixLine
	next        int
	seq         uint64
	prs         map[string]*matrixPRState
	totalHashes int
	rateStart   time.Time
	rateCount   int
	rateDropped uint64
	notify      func(seq uint64)
}

func newMatrixSource(cfg config.MatrixConfig, logger *slog.Logger) *matrixSource {
	if logger == nil {
		logger = slog.Default()
	}
	m := &matrixSource{
		in:     make(chan github.SentinelPatchObservation, matrixIngestCap),
		logger: logger,
		now:    time.Now,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		ring:   make([]matrixLine, 0, matrixBacklogCap),
		prs:    make(map[string]*matrixPRState),
	}
	m.setConfig(cfg)
	return m
}

func (m *matrixSource) setConfig(cfg config.MatrixConfig) {
	c := cfg
	c.Repos = append([]string(nil), cfg.Repos...)
	c.ExcludePaths = append([]string(nil), cfg.ExcludePaths...)
	m.cfg.Store(&c)
}

// admits applies the observation-level gates: Matrix active, hive-identified
// author, public repo on the explicit allowlist. There is no way to widen it.
func (m *matrixSource) admits(obs github.SentinelPatchObservation) bool {
	cfg := m.cfg.Load()
	return cfg != nil && cfg.Active() && obs.HiveIdentified && !obs.Private && cfg.RepoAllowed(obs.Repo)
}

// Observe is the sentinel PatchObserver. It gates the observation, then hands
// it off without blocking: when the ingest channel is full the observation
// is dropped and counted.
func (m *matrixSource) Observe(obs github.SentinelPatchObservation) {
	if !m.admits(obs) {
		return
	}
	select {
	case m.in <- obs:
	default:
		m.dropped.Add(1)
	}
}

// Dropped counts observations Observe discarded because the channel was full.
func (m *matrixSource) Dropped() uint64 { return m.dropped.Load() }

func (m *matrixSource) start() {
	m.startOnce.Do(func() { go m.run() })
}

func (m *matrixSource) close() {
	m.stopOnce.Do(func() { close(m.stop) })
}

func (m *matrixSource) run() {
	defer close(m.done)
	for {
		select {
		case <-m.stop:
			return
		case obs := <-m.in:
			m.safeIngest(obs)
		}
	}
}

func (m *matrixSource) safeIngest(obs github.SentinelPatchObservation) {
	defer func() {
		if r := recover(); r != nil {
			m.logger.Warn("matrix line source dropped an observation after a panic", "repo", obs.Repo, "number", obs.Number, "panic", fmt.Sprint(r))
		}
	}()
	m.ingest(obs)
}

// ingest filters one observation into the backlog and returns how many lines
// it added.
func (m *matrixSource) ingest(obs github.SentinelPatchObservation) int {
	if !m.admits(obs) {
		return 0
	}
	excludes := m.cfg.Load().ExcludePaths
	now := m.now()
	at := obs.At
	if at.IsZero() {
		at = now
	}
	key := obs.Repo + "#" + strconv.Itoa(obs.Number)

	m.mu.Lock()
	m.pruneLocked(now)
	st := m.prs[key]
	if st == nil {
		st = &matrixPRState{hashes: make(map[[sha256.Size]byte]time.Time)}
		m.prs[key] = st
	}
	st.touched = now
	if st.head != obs.HeadSHA {
		st.head = obs.HeadSHA
		st.headCount = 0
	}
	added := 0
	files := obs.Files
	if len(files) > matrixMaxFilesPerObs {
		files = files[:matrixMaxFilesPerObs]
	}
scan:
	for _, f := range files {
		if matrixPathDenied(f.Path, excludes) || len(f.Patch) > matrixMaxPatchBytes {
			continue
		}
		lang := matrixLang(f.Path)
		for _, raw := range matrixAddedLines(f.Patch) {
			if st.headCount >= matrixMaxLinesPerHead {
				break scan
			}
			line, ok := matrixAcceptLine(raw)
			if !ok {
				continue
			}
			h := sha256.Sum256([]byte(f.Path + "\x00" + line))
			if seen, dup := st.hashes[h]; dup && now.Sub(seen) < matrixDedupTTL {
				continue
			}
			if !m.rateAllowLocked(now) {
				break scan
			}
			if _, dup := st.hashes[h]; !dup {
				m.totalHashes++
			}
			st.hashes[h] = now
			st.headCount++
			m.seq++
			m.appendLocked(matrixLine{Seq: m.seq, Repo: obs.Repo, PR: obs.Number, Head: obs.HeadSHA, Path: f.Path, Lang: lang, Line: line, At: at})
			added++
		}
	}
	m.evictLocked(key)
	seq, notify := m.seq, m.notify
	m.mu.Unlock()

	if added > 0 && notify != nil {
		notify(seq)
	}
	return added
}

func (m *matrixSource) appendLocked(l matrixLine) {
	if len(m.ring) < matrixBacklogCap {
		m.ring = append(m.ring, l)
		return
	}
	m.ring[m.next] = l
	m.next = (m.next + 1) % matrixBacklogCap
}

// rateAllowLocked admits at most matrixMaxLinesPerRate lines per window.
func (m *matrixSource) rateAllowLocked(now time.Time) bool {
	if now.Sub(m.rateStart) >= matrixRateWindow || now.Before(m.rateStart) {
		m.rateStart = now
		m.rateCount = 0
	}
	if m.rateCount >= matrixMaxLinesPerRate {
		m.rateDropped++
		return false
	}
	m.rateCount++
	return true
}

// pruneLocked forgets PR sets untouched for a full dedup TTL.
func (m *matrixSource) pruneLocked(now time.Time) {
	for key, st := range m.prs {
		if now.Sub(st.touched) >= matrixDedupTTL {
			m.removeLocked(key)
		}
	}
}

// evictLocked drops the least recently touched PR sets until both the PR
// count and the global hash count are within bounds. keep is the PR just
// ingested; it goes last.
func (m *matrixSource) evictLocked(keep string) {
	for len(m.prs) > matrixMaxTrackedPRs || m.totalHashes > matrixMaxDedupHashes {
		oldest := ""
		var oldestAt time.Time
		for key, st := range m.prs {
			if key == keep && len(m.prs) > 1 {
				continue
			}
			if oldest == "" || st.touched.Before(oldestAt) {
				oldest, oldestAt = key, st.touched
			}
		}
		if oldest == "" {
			return
		}
		m.removeLocked(oldest)
	}
}

func (m *matrixSource) removeLocked(key string) {
	if st, ok := m.prs[key]; ok {
		m.totalHashes -= len(st.hashes)
		delete(m.prs, key)
	}
}

// Since returns up to limit backlog lines with Seq > seq, oldest first, and the
// latest sequence number. limit <= 0 means a default page.
func (m *matrixSource) Since(seq uint64, limit int) ([]matrixLine, uint64) {
	if limit <= 0 {
		limit = matrixDefaultSinceMax
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.ring)
	out := make([]matrixLine, 0, min(limit, n))
	for i := 0; i < n && len(out) < limit; i++ {
		l := m.ring[(m.next+i)%n]
		if l.Seq > seq {
			out = append(out, l)
		}
	}
	return out, m.seq
}

// Latest is the sequence number of the newest accepted line; 0 when none.
func (m *matrixSource) Latest() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}

// reset forgets the backlog and dedup state; Seq keeps counting so a reader
// never sees a sequence number reused.
func (m *matrixSource) reset() {
	m.mu.Lock()
	m.ring = m.ring[:0]
	m.next = 0
	m.prs = make(map[string]*matrixPRState)
	m.totalHashes = 0
	m.mu.Unlock()
}

func (m *matrixSource) setNotify(fn func(seq uint64)) {
	m.mu.Lock()
	m.notify = fn
	m.mu.Unlock()
}

// MatrixObserver returns the sentinel PatchObserver feeding the Matrix line
// source, or nil when dashboard.matrix is not active — then nothing is
// created, and a source left over from an earlier
// active config stops admitting and forgets its backlog. The source is built on first use and
// later calls refresh its allowlist and exclude globs.
func (s *Server) MatrixObserver(cfg config.MatrixConfig) func(github.SentinelPatchObservation) {
	if s == nil {
		return nil
	}
	s.matrixMu.Lock()
	defer s.matrixMu.Unlock()
	if !cfg.Active() {
		if s.matrix != nil {
			s.matrix.setConfig(cfg)
			s.matrix.reset()
		}
		return nil
	}
	if s.matrix == nil {
		s.matrix = newMatrixSource(cfg, s.logger)
		s.matrix.setNotify(s.nudgeMatrixLines)
		s.matrix.start()
	} else {
		s.matrix.setConfig(cfg)
		s.matrix.setNotify(s.nudgeMatrixLines)
	}
	return s.matrix.Observe
}
