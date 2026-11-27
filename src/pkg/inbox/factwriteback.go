package inbox

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/atomicfile"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

const (
	// factFileMode matches the mode connector facts are written with.
	factFileMode = 0o644
	// factTranscriptProviderKey names the provider/model that made a
	// machine transcript written into a fact.
	factTranscriptProviderKey = "attr_transcript_provider"
	// factTranscriptLanguageKey is the language the provider reported.
	factTranscriptLanguageKey = "attr_transcript_language"
	// factTranscribedAtKey is when the machine transcript finished.
	factTranscribedAtKey = "attr_transcribed_at"
	// factHighlightsHeading opens the highlights section the machine
	// transcript is placed in front of.
	factHighlightsHeading = "## Highlights"
	// factDateLayout is the day shown in the machine-transcript note.
	factDateLayout = "2006-01-02"
	// hiveTranscriptProvenancePrefix opens the provenance line of a
	// transcript of audio this hive stores itself (hive-recording, upload).
	hiveTranscriptProvenancePrefix = "Transcript made by this hive with"
	// machineTranscriptProvenancePrefix opens the provenance line of a
	// machine transcript of a third-party source's recording.
	machineTranscriptProvenancePrefix = "Machine transcript made by this hive with"
)

// factPendingNotes are the pending lines connectors write into a fact with
// no text; the machine transcript replaces them.
var factPendingNotes = []string{
	"_" + connector.PlaudPendingText + "_",
	"_" + connector.MeetingPendingText + "_",
	"_" + RecordingPendingText + "_",
}

// factState is the Item.FactState of a fact: its recorded content state, or
// for facts written before connectors recorded one, pending when the fact
// has no summary and no transcript and ready otherwise.
func factState(fm map[string]string, pending bool) string {
	if st := connector.FactContentState(fm); st != "" {
		return st
	}
	if pending {
		return connector.FactContentPending
	}
	return connector.FactContentReady
}

// writeMachineFact writes a done machine transcript into the item's
// Knowledge fact while the source still has no text, so knowledge search,
// nous and agents see the words, not just the Inbox. It reports whether the
// fact changed. Items without a fact (not synced yet) are left for a later
// listing; facts that already carry text (source text, or this machine
// transcript) are never touched.
func (s *Service) writeMachineFact(it Item, rec TranscriptRecord) (bool, error) {
	if it.FactPath == "" || rec.Status != TranscriptionDone || strings.TrimSpace(rec.Text) == "" {
		return false, nil
	}
	s.factMu.Lock()
	defer s.factMu.Unlock()
	data, err := os.ReadFile(it.FactPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	out, ok := machineFact(string(data), it, rec)
	if !ok {
		return false, nil
	}
	if err := writeFactAtomic(it.FactPath, out); err != nil {
		return false, err
	}
	return true, nil
}

// machineFact returns content with the machine transcript written in, or
// ok=false when the fact must stay as it is.
func machineFact(content string, it Item, rec TranscriptRecord) (string, bool) {
	if !strings.HasPrefix(content, "---\n") {
		return "", false
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return "", false
	}
	fm, body := splitFrontMatter(content)
	if fm["status"] == "deprecated" || connector.FactContentState(fm) == connector.FactContentMachine {
		return "", false
	}
	sections := splitSections(body)
	if sections["summary"] != "" || sections["transcript"] != "" {
		return "", false
	}

	contentKey := connector.FactContentKey(fm)
	if contentKey == "" {
		contentKey = "attr_" + strings.ReplaceAll(factKeyPart(it.Source), "-", "_") + "_content"
	}
	set := map[string]string{
		contentKey:                connector.FactContentMachine,
		factTranscriptProviderKey: rec.Provider + "/" + rec.Model,
		factTranscribedAtKey:      rec.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if rec.Language != "" {
		set[factTranscriptLanguageKey] = rec.Language
	}
	head := setFrontMatter(strings.Split(content[4:4+end], "\n"), set, []string{contentKey, factTranscriptProviderKey, factTranscriptLanguageKey, factTranscribedAtKey})

	return "---\n" + strings.Join(head, "\n") + "\n---\n\n" + machineFactBody(body, it, rec) + "\n", true
}

// setFrontMatter replaces the lines of keys in set and appends the missing
// ones, in order, before synthesized/synced_at (or at the end).
func setFrontMatter(lines []string, set map[string]string, order []string) []string {
	done := map[string]bool{}
	out := make([]string, 0, len(lines)+len(set))
	for _, l := range lines {
		k, _, ok := strings.Cut(l, ":")
		k = strings.TrimSpace(k)
		if v, want := set[k]; ok && want {
			out = append(out, k+": "+knowledge.SanitizeFrontmatterValue(v))
			done[k] = true
			continue
		}
		out = append(out, l)
	}
	at := len(out)
	for i, l := range out {
		if strings.HasPrefix(l, "synthesized:") || strings.HasPrefix(l, "synced_at:") {
			at = i
			break
		}
	}
	var add []string
	for _, k := range order {
		if v, want := set[k]; want && !done[k] {
			add = append(add, k+": "+knowledge.SanitizeFrontmatterValue(v))
		}
	}
	return append(out[:at:at], append(add, out[at:]...)...)
}

// machineFactBody drops the pending note and places the machine transcript,
// fenced as untrusted data, ahead of the highlights (or at the end). The
// metadata bullets and highlights are kept as they are.
func machineFactBody(body string, it Item, rec TranscriptRecord) string {
	var kept []string
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		if isPendingNote(l) {
			continue
		}
		kept = append(kept, l)
	}
	body = strings.Join(kept, "\n")
	label := it.Source
	if label == "" {
		label = "the source"
	}
	section := "## " + rec.Label() + "\n\n" + machineProvenance(it.Source, label, rec) + "\n\n" +
		WrapUntrusted(it.Source, it.ID, rec.Text) + "\n"
	if i := strings.Index(body, "\n"+factHighlightsHeading); i >= 0 {
		return collapseBlankLines(strings.TrimRight(body[:i], "\n") + "\n\n" + section + "\n" + strings.TrimLeft(body[i:], "\n"))
	}
	return collapseBlankLines(strings.TrimRight(body, "\n") + "\n\n" + section)
}

// machineProvenance is the italic line above a machine transcript. Audio
// this hive keeps itself (its own recordings, dropped audio files) has no
// other text to wait for, so the line only names the provider and the day;
// a third-party source (Plaud, Zoom, ...) may still produce its own text,
// which then replaces the machine transcript.
func machineProvenance(source, label string, rec TranscriptRecord) string {
	day := rec.UpdatedAt.UTC().Format(factDateLayout)
	if hiveStoredAudioSource(source) {
		return fmt.Sprintf("_%s %s/%s on %s._", hiveTranscriptProvenancePrefix, rec.Provider, rec.Model, day)
	}
	return fmt.Sprintf("_%s %s/%s on %s, because %s has no text for this recording yet. It is replaced when %s produces its own transcript or summary._",
		machineTranscriptProvenancePrefix, rec.Provider, rec.Model, day, label, label)
}

// hiveStoredAudioSource reports a source whose audio is stored on this hive
// and whose only text is this hive's transcript.
func hiveStoredAudioSource(source string) bool {
	return source == RecordingType || source == UploadType
}

// isMachineProvenance reports the italic provenance line machineProvenance
// writes, so displays (the Inbox excerpt) can leave it out.
func isMachineProvenance(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "_"+hiveTranscriptProvenancePrefix+" ") || strings.HasPrefix(line, "_"+machineTranscriptProvenancePrefix+" ")
}

func isPendingNote(line string) bool {
	line = strings.TrimSpace(line)
	for _, n := range factPendingNotes {
		if line == n {
			return true
		}
	}
	return false
}

// collapseBlankLines squeezes runs of blank lines left by the removed note.
func collapseBlankLines(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

// factKeyPart reduces a source type to [a-z0-9-] for an attr key.
func factKeyPart(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "source"
	}
	return b.String()
}

// writeFactAtomic writes a fact through a temp file and a rename, with the
// mode connector facts use.
func writeFactAtomic(path, content string) error {
	return atomicfile.WriteFile(path, []byte(content), factFileMode)
}
