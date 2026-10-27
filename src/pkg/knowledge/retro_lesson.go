package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hivecommons/hive/crux/logscrub"
)

const (
	RetroLessonMinChars   = 25
	RetroLessonMaxChars   = 500
	retroLessonConfidence = 0.7
	retroLessonLayer      = "project"
	retroLessonSearchCap  = 5
	retroLessonSlugPrefix = "retro-lesson-"
	retroLessonMaxSources = 20
	retroLessonLockFile   = ".retro-lessons.lock"

	// DefaultRetroLessonConfidenceStep is how much each recorded repeat of a
	// retro lesson raises its stored confidence.
	DefaultRetroLessonConfidenceStep = 0.05
	// DefaultRetroLessonConfidenceCap bounds the stored confidence repeats can
	// raise a retro lesson to.
	DefaultRetroLessonConfidenceCap = 0.9
)

// retroLessonRepeatMu serializes repeat updates inside one process; the flock
// on retroLessonLockFile serializes them across processes.
var retroLessonRepeatMu sync.Mutex

// RetroLesson is a durable, model-generated process lesson produced by the
// retro lane from a completed bead/PR trajectory.
type RetroLesson struct {
	Lesson     string
	SourceBead string
	SourcePR   string
	SourceDate time.Time
	// ConfidenceStep and ConfidenceCap tune how far each recorded repeat
	// raises the matching lesson's stored confidence. Zero selects
	// DefaultRetroLessonConfidenceStep and DefaultRetroLessonConfidenceCap.
	ConfidenceStep float64
	ConfidenceCap  float64
}

// IngestRetroLesson quality-gates, deduplicates, and stores one retro lesson
// as a normal knowledge fact. It returns created=false for rejected or duplicate
// lessons so retro analysis can fail open without affecting deterministic beads.
// A duplicate of an existing retro lesson is recorded on that lesson as a
// repeat (see recordRetroLessonRepeat) instead of being discarded.
func (k *KnowledgeAPI) IngestRetroLesson(ctx context.Context, lesson RetroLesson) (slug string, created bool, err error) {
	if k == nil {
		return "", false, nil
	}
	body := strings.TrimSpace(lesson.Lesson)
	if !ValidRetroLesson(body) {
		return "", false, nil
	}
	key := normalizedRetroLessonKey(body)
	slug = retroLessonSlugPrefix + shortHash(key)
	if _, err := k.VaultFact(slug); err == nil {
		k.recordRetroLessonRepeat(slug, lesson)
		return slug, false, nil
	}
	for _, existing := range k.SearchAllWithVaults(ctx, body, "", retroLessonSearchCap) {
		if nearRetroLessonKey(normalizedRetroLessonKey(existing.Body), key) || nearRetroLessonKey(normalizedRetroLessonKey(existing.Title), key) {
			k.recordRetroLessonRepeat(existing.Slug, lesson)
			return existing.Slug, false, nil
		}
	}

	title := retroLessonTitle(body)
	fact := ExtractedFact{
		Title:      title,
		Body:       body,
		Type:       FactPattern,
		Confidence: retroLessonConfidence,
		Tags:       []string{"retro", "lesson"},
		SourcePR:   retroSource(lesson),
		SourceDate: lesson.SourceDate,
	}
	if fact.SourceDate.IsZero() {
		fact.SourceDate = time.Now().UTC()
	}

	if client := k.clientForLayer(LayerType(retroLessonLayer)); client != nil {
		if err := client.IngestFacts(ctx, []ExtractedFact{fact}); err != nil {
			return "", false, err
		}
		k.addRetroGraphTriple(slug, lesson)
		return slug, true, nil
	}

	if err := k.writeRetroLessonFile(slug, fact); err != nil {
		return "", false, err
	}
	k.addRetroGraphTriple(slug, lesson)
	return slug, true, nil
}

// recordRetroLessonRepeat records lesson as a repeat observation of the
// existing retro lesson slug: its source joins the lesson's sources list, its
// observation count rises by one and its stored confidence rises by one step
// up to the cap. Only the confidence, observations and sources front-matter
// lines change; the body, lifecycle state and every other line are preserved.
// Facts that are not retro lessons, superseded or deprecated lessons, and
// repeats from an already recorded source bead are left untouched. Failures
// are logged, never returned: a lost repeat must not fail the retro lane.
func (k *KnowledgeAPI) recordRetroLessonRepeat(slug string, lesson RetroLesson) {
	if !strings.HasPrefix(slug, retroLessonSlugPrefix) {
		return
	}
	if client := k.clientForLayer(LayerType(retroLessonLayer)); client != nil {
		// The remote page API has no conditional or partial update, so a repeat
		// could overwrite a concurrent change or drop front matter it does not
		// carry. Skip rather than risk a partly written lesson.
		k.logger.Info("retro: lesson repeat skipped: remote knowledge layer cannot update the lesson atomically",
			"slug", slug, "source_bead", lesson.SourceBead, "source_pr", lesson.SourcePR)
		return
	}
	entry := retroRepeatSource(lesson)
	if entry == "" {
		return
	}
	dir := filepath.Join(knowledgeBaseDir, retroLessonLayer)
	path := filepath.Join(dir, strings.ReplaceAll(slug+".md", "/", "_"))
	if _, err := os.Stat(path); err != nil {
		return
	}

	unlock := lockRetroLessonDir(dir)
	raw, err := os.ReadFile(path)
	if err != nil {
		unlock()
		k.logger.Warn("retro: lesson repeat skipped: reading lesson failed", "slug", slug, "error", err)
		return
	}
	updated, observations, ok := applyRetroLessonRepeat(string(raw), slug, entry, retroSourceToken(lesson.SourceBead), lesson.ConfidenceStep, lesson.ConfidenceCap)
	if !ok {
		unlock()
		return
	}
	err = writeFileAtomic(dir, path, []byte(updated))
	unlock()
	if err != nil {
		k.logger.Warn("retro: lesson repeat skipped: writing lesson failed", "slug", slug, "error", err)
		return
	}
	k.triggerVaultReindex(dir)
	k.addRetroGraphTriple(slug, lesson)
	k.logger.Info("retro: lesson repeat recorded", "slug", slug, "source_bead", lesson.SourceBead, "observations", observations)
}

// applyRetroLessonRepeat returns content with one repeat from entry recorded,
// the new observation count, and false when nothing should be written: the
// content is not a retro lesson, the lesson is superseded or deprecated, or
// the source bead (or, without a bead, the exact source) is already recorded.
func applyRetroLessonRepeat(content, slug, entry, bead string, step, maxConf float64) (string, int, bool) {
	if !strings.HasPrefix(content, "---\n") {
		return "", 0, false
	}
	endIdx := strings.Index(content[4:], "\n---")
	if endIdx <= 0 {
		return "", 0, false
	}
	parsed := parseObsidianFileDetailed(content, slug)
	if !hasTag(parsed.tags, "retro") || !hasTag(parsed.tags, "lesson") {
		return "", 0, false
	}
	switch resolveLifecycleState(parsed.state, parsed.status, parsed.supersededBy) {
	case StateApproved, StateDraft:
	default:
		return "", 0, false
	}

	lines := strings.Split(content[4:4+endIdx], "\n")
	observations := 1
	var sources []string
	hasSources := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(trimmed, "observations:"); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 1 {
				observations = n
			}
		}
		if v, ok := strings.CutPrefix(trimmed, "sources:"); ok {
			hasSources = true
			sources = parseInlineFrontmatterList(v)
		}
	}
	if !hasSources {
		// Lessons written before repeats were counted carry only `source:`.
		if legacy := strings.TrimSpace(strings.TrimPrefix(parsed.source, "retro")); legacy != "" {
			sources = []string{legacy}
		}
	}
	for _, s := range sources {
		if bead != "" {
			for _, field := range strings.Fields(s) {
				if field == "bead:"+bead {
					return "", 0, false
				}
			}
		} else if s == entry {
			return "", 0, false
		}
	}
	sources = append(sources, entry)
	if len(sources) > retroLessonMaxSources {
		sources = sources[len(sources)-retroLessonMaxSources:]
	}
	observations++

	conf := parsed.confidence
	if conf < 0 {
		conf = retroLessonConfidence
	}
	conf = nextRetroLessonConfidence(conf, step, maxConf)

	confLine := fmt.Sprintf("confidence: %.2f", conf)
	obsLine := fmt.Sprintf("observations: %d", observations)
	srcLine := fmt.Sprintf("sources: [%s]", sanitizeFrontmatterList(sources))
	hasObservations := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "observations:") {
			hasObservations = true
		}
	}
	out := make([]string, 0, len(lines)+2)
	wroteConf, wroteObs, wroteSrc := false, false, false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "confidence:"):
			out = append(out, confLine)
			wroteConf = true
			if !hasObservations && !wroteObs {
				out = append(out, obsLine)
				wroteObs = true
			}
		case strings.HasPrefix(trimmed, "observations:"):
			out = append(out, obsLine)
			wroteObs = true
		case strings.HasPrefix(trimmed, "sources:"):
			out = append(out, srcLine)
			wroteSrc = true
		case strings.HasPrefix(trimmed, "source:"):
			out = append(out, line)
			if !hasSources && !wroteSrc {
				out = append(out, srcLine)
				wroteSrc = true
			}
		default:
			out = append(out, line)
		}
	}
	if !wroteConf {
		out = append(out, confLine)
	}
	if !wroteObs {
		out = append(out, obsLine)
	}
	if !wroteSrc {
		out = append(out, srcLine)
	}
	return "---\n" + strings.Join(out, "\n") + content[4+endIdx:], observations, true
}

// nextRetroLessonConfidence raises conf by step without passing maxConf. A
// lesson already at or above the cap keeps its confidence.
func nextRetroLessonConfidence(conf, step, maxConf float64) float64 {
	if step <= 0 {
		step = DefaultRetroLessonConfidenceStep
	}
	if maxConf <= 0 {
		maxConf = DefaultRetroLessonConfidenceCap
	}
	maxConf = math.Min(maxConf, 1)
	if conf >= maxConf {
		return conf
	}
	next := math.Round(math.Min(conf+step, maxConf)*100) / 100
	if next > maxConf {
		next = math.Floor(maxConf*100+1e-9) / 100
	}
	return next
}

func parseInlineFrontmatterList(v string) []string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "[") {
		return nil
	}
	var out []string
	for _, item := range strings.Split(strings.Trim(v, "[]"), ",") {
		item = strings.Trim(strings.TrimSpace(item), "\"'")
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// retroRepeatSource is the sources-list entry for one repeat, e.g.
// "bead:b-1 pr:org/repo#2". Empty when the lesson names no source.
func retroRepeatSource(lesson RetroLesson) string {
	var parts []string
	if bead := retroSourceToken(lesson.SourceBead); bead != "" {
		parts = append(parts, "bead:"+bead)
	}
	if pr := retroSourceToken(lesson.SourcePR); pr != "" {
		parts = append(parts, "pr:"+pr)
	}
	return strings.Join(parts, " ")
}

var retroSourceListReplacer = strings.NewReplacer(",", " ", "[", " ", "]", " ", "'", " ", "\"", " ")

// retroSourceToken sanitizes a bead or PR reference into one whitespace-free
// token that is safe inside an inline front-matter list.
func retroSourceToken(v string) string {
	v = retroSourceListReplacer.Replace(sanitizeFrontmatterValue(v))
	return strings.Join(strings.Fields(v), "_")
}

// lockRetroLessonDir serializes repeat read-modify-write cycles on the retro
// lesson directory within this process and, best effort, across processes.
func lockRetroLessonDir(dir string) func() {
	retroLessonRepeatMu.Lock()
	f, err := os.OpenFile(filepath.Join(dir, retroLessonLockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return retroLessonRepeatMu.Unlock
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return retroLessonRepeatMu.Unlock
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		retroLessonRepeatMu.Unlock()
	}
}

func writeFileAtomic(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".retro-lesson-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func ValidRetroLesson(lesson string) bool {
	lesson = strings.TrimSpace(lesson)
	n := len([]rune(lesson))
	if n < RetroLessonMinChars || n > RetroLessonMaxChars {
		return false
	}
	return !logscrub.TokenPattern.MatchString(lesson)
}

func (k *KnowledgeAPI) writeRetroLessonFile(slug string, fact ExtractedFact) error {
	dir := filepath.Join(knowledgeBaseDir, retroLessonLayer)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating retro lesson dir: %w", err)
	}
	path := filepath.Join(dir, strings.ReplaceAll(slug+".md", "/", "_"))
	var buf strings.Builder
	buf.WriteString("---\n")
	fmt.Fprintf(&buf, "title: %s\n", sanitizeFrontmatterValue(fact.Title))
	fmt.Fprintf(&buf, "type: %s\n", sanitizeFrontmatterValue(string(fact.Type)))
	fmt.Fprintf(&buf, "layer: %s\n", retroLessonLayer)
	fmt.Fprintf(&buf, "confidence: %.2f\n", fact.Confidence)
	fmt.Fprintf(&buf, "tags: [%s]\n", sanitizeFrontmatterList(fact.Tags))
	fmt.Fprintf(&buf, "source: %s\n", sanitizeFrontmatterValue(fact.SourcePR))
	fmt.Fprintf(&buf, "synthesized: %s\n", fact.SourceDate.UTC().Format(time.RFC3339))
	buf.WriteString("---\n\n")
	buf.WriteString(fact.Body)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("writing retro lesson file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming retro lesson file: %w", err)
	}
	k.triggerVaultReindex(dir)
	return nil
}

func (k *KnowledgeAPI) addRetroGraphTriple(slug string, lesson RetroLesson) {
	gs := k.GraphStore()
	if gs == nil {
		return
	}
	source := strings.TrimSpace(lesson.SourcePR)
	if source == "" {
		source = strings.TrimSpace(lesson.SourceBead)
	}
	if source == "" {
		return
	}
	if err := gs.AddTriple(Triple{Subject: slug, Predicate: PredicateDerivedFrom, Object: "retro:" + source}); err != nil {
		k.logger.Warn("failed to add retro lesson graph triple", "slug", slug, "source", source, "error", err)
	}
}

func retroLessonTitle(body string) string {
	words := strings.Fields(body)
	if len(words) > 12 {
		words = words[:12]
	}
	title := strings.Join(words, " ")
	title = strings.Trim(title, " .")
	if title == "" {
		title = "Generalized retro lesson"
	}
	return "Retro lesson: " + title
}

var nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)

func normalizedRetroLessonKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonAlphaNum.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

func nearRetroLessonKey(existing, candidate string) bool {
	if existing == "" || candidate == "" {
		return false
	}
	return existing == candidate || strings.Contains(existing, candidate) || strings.Contains(candidate, existing)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func retroSource(lesson RetroLesson) string {
	parts := []string{"retro"}
	if lesson.SourceBead != "" {
		parts = append(parts, "bead:"+lesson.SourceBead)
	}
	if lesson.SourcePR != "" {
		parts = append(parts, "pr:"+lesson.SourcePR)
	}
	return strings.Join(parts, " ")
}
