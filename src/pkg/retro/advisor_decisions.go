package retro

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Knowledge-advisor decision store (hivecommons/hive#17211). The ranking
// decorator writes one compact record per decision; the retro lane reads them
// back through AdvisorDecisionReader, joined on the kick's primed issue refs.
// Prompt history records deliveries only, so holdout decisions would never
// reach the lane through it.
const (
	// AdvisorDecisionsPath is the JSONL file on the spoke's PVC, rotated
	// alongside it like prompt-history.jsonl.
	AdvisorDecisionsPath = "/data/knowledge-advisor-decisions.jsonl"

	// A record is ~250 bytes. The advisor is capped at 300 model calls per
	// fleet day, so 5 MiB holds roughly two months of decisions; the backups
	// and age bound keep the store small next to prompt history.
	advisorDecisionsMaxSizeMB  = 5
	advisorDecisionsMaxBackups = 3
	advisorDecisionsMaxAgeDays = 90

	// advisorDecisionsRingCap is how many records are held in memory for the
	// lane's reads: about two months at the fleet-day ceiling.
	advisorDecisionsRingCap = 20000
)

// AdvisorDecisionRecord is one persisted advisor decision. It deliberately has
// no reason, prompt or pane text field: only what the arm comparison needs.
type AdvisorDecisionRecord struct {
	Timestamp string   `json:"ts"`
	Agent     string   `json:"agent"`
	Kick      string   `json:"kick"`
	Refs      []string `json:"refs,omitempty"`
	Mode      string   `json:"mode"`
	Arm       string   `json:"arm"`
	Outcome   string   `json:"outcome"`
	Slug      string   `json:"slug,omitempty"`
}

// AdvisorDecisionArm is the arm a decision belongs to: holdout when the kick
// was held out, shadow in shadow mode, deliver otherwise.
func AdvisorDecisionArm(mode, outcome string) string {
	if outcome == "held-out" {
		return AdvisorArmHoldout
	}
	return armOfMode(mode)
}

// AdvisorDecisionStore is a durable, bounded store of advisor decisions. It
// implements AdvisorDecisionReader.
type AdvisorDecisionStore struct {
	mu     sync.Mutex
	writer *lumberjack.Logger
	ring   []AdvisorDecisionRecord
}

// OpenAdvisorDecisionStore opens the store at AdvisorDecisionsPath when the
// PVC is mounted and is purely in-memory otherwise, mirroring prompt history.
func OpenAdvisorDecisionStore() *AdvisorDecisionStore {
	if _, err := os.Stat("/data"); err == nil {
		return NewAdvisorDecisionStore(AdvisorDecisionsPath)
	}
	return NewAdvisorDecisionStore("")
}

// NewAdvisorDecisionStore opens a store backed by path; an empty path keeps it
// in memory only.
func NewAdvisorDecisionStore(path string) *AdvisorDecisionStore {
	s := &AdvisorDecisionStore{}
	if path != "" {
		s.writer = &lumberjack.Logger{
			Filename:   path,
			MaxSize:    advisorDecisionsMaxSizeMB,
			MaxBackups: advisorDecisionsMaxBackups,
			MaxAge:     advisorDecisionsMaxAgeDays,
			Compress:   true,
		}
		s.loadFromDisk(path)
	}
	return s
}

// loadFromDisk repopulates the ring from the active JSONL file, skipping
// malformed lines such as a final line truncated by a kill mid-write.
func (s *AdvisorDecisionStore) loadFromDisk(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec AdvisorDecisionRecord
		if json.Unmarshal(line, &rec) != nil || rec.Agent == "" || rec.Kick == "" {
			continue
		}
		s.ring = append(s.ring, rec)
	}
	if len(s.ring) > advisorDecisionsRingCap {
		s.ring = s.ring[len(s.ring)-advisorDecisionsRingCap:]
	}
}

// Record appends one decision. A missing timestamp or arm is filled in.
func (s *AdvisorDecisionStore) Record(rec AdvisorDecisionRecord) {
	if s == nil || rec.Agent == "" || rec.Kick == "" {
		return
	}
	if rec.Timestamp == "" {
		rec.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if rec.Arm == "" {
		rec.Arm = AdvisorDecisionArm(rec.Mode, rec.Outcome)
	}
	rec.Refs = append([]string(nil), rec.Refs...)
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring = append(s.ring, rec)
	if len(s.ring) > advisorDecisionsRingCap {
		s.ring = append(s.ring[:0:0], s.ring[len(s.ring)-advisorDecisionsRingCap:]...)
	}
	if s.writer != nil {
		_, _ = s.writer.Write(append(line, '\n'))
	}
}

// AdvisorDecisions returns the decisions whose kick primed issueRef. A stored
// ref without an owner ("repo#N") matches the issue ref's repo name.
func (s *AdvisorDecisionStore) AdvisorDecisions(issueRef string) []AdvisorDecision {
	if s == nil {
		return nil
	}
	want := strings.ToLower(strings.TrimSpace(issueRef))
	if want == "" {
		return nil
	}
	short := want
	if i := strings.Index(want, "/"); i >= 0 {
		short = want[i+1:]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AdvisorDecision
	for _, rec := range s.ring {
		for _, ref := range rec.Refs {
			ref = strings.ToLower(ref)
			if ref == want || ref == short {
				out = append(out, AdvisorDecision{Kick: rec.Kick, Mode: rec.Mode, Outcome: rec.Outcome})
				break
			}
		}
	}
	return out
}

// Close flushes and closes the backing file.
func (s *AdvisorDecisionStore) Close() error {
	if s == nil || s.writer == nil {
		return nil
	}
	return s.writer.Close()
}
