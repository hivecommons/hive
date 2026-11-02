package timeline

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const observationTestTimeout = 5 * time.Second

func TestRecordObservationKeepsDistinctPRsAndTimeCorrections(t *testing.T) {
	s := NewStore()
	const ref = "org/repo#1"
	first := Event{IssueRef: ref, Kind: KindMerged, At: 1234, Attrs: map[string]string{AttrPRRef: "org/repo#2"}}
	s.RecordObservation(first)
	other := first
	other.Attrs = map[string]string{AttrPRRef: "org/repo#3"}
	s.RecordObservation(other)
	earlier := other
	earlier.At--
	s.RecordObservation(earlier)
	later := other
	later.At++
	s.RecordObservation(later)
	j, _ := s.Journey(ref)
	st := j.Stages[KindMerged]
	if st.Count != 4 || st.FirstAt != earlier.At || st.LastAt != later.At || st.Attrs[AttrPRRef] != "org/repo#3" {
		t.Fatalf("distinct linked PR or corrected timestamp was deduplicated: %+v", st)
	}
}

func waitObservation(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(observationTestTimeout):
		t.Fatal("timeline observation did not reach synchronization point")
	}
}

func TestRecordObservationPersistsFirstAndCorrectionsNotReplays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timeline.json")
	s := NewStore()
	if err := s.EnablePersistence(path, nil); err != nil {
		t.Fatal(err)
	}
	writes := 0
	s.writeFile = func(path string, data []byte, subject string) error {
		writes++
		return writeFileAtomic(path, data, subject)
	}
	e := Event{IssueRef: "org/repo#1", Kind: KindMerged, At: 1234, Attrs: map[string]string{"actor": "unknown"}}
	s.RecordObservation(e)
	s.RecordObservation(e)
	j, ok := s.Journey(e.IssueRef)
	if !ok || j.Stages[KindMerged].Count != 1 || writes != 1 {
		t.Fatalf("identical observation counted/wrote again: journey=%+v writes=%d", j, writes)
	}
	e.Attrs = map[string]string{"actor": "human"}
	s.RecordObservation(e)
	if writes != 2 {
		t.Fatalf("corrected precious metadata was not durable: writes=%d", writes)
	}
	restarted := NewStore()
	if err := restarted.EnablePersistence(path, nil); err != nil {
		t.Fatal(err)
	}
	restarted.writeFile = func(string, []byte, string) error {
		t.Error("unchanged restart replay persisted again")
		return nil
	}
	restarted.RecordObservation(e)
	j, _ = restarted.Journey(e.IssueRef)
	if st := j.Stages[KindMerged]; st.Count != 2 || st.Attrs["actor"] != "human" || st.FirstAt != e.At {
		t.Fatalf("first/corrected observation did not survive restart: %+v", st)
	}
	s.Record(e)
	j, _ = s.Journey(e.IssueRef)
	if j.Stages[KindMerged].Count != 3 || writes != 3 {
		t.Fatal("ordinary Record lost its repeated-event semantics")
	}
}

func TestRecordObservationFailedPersistenceRetriesUnchangedEvidence(t *testing.T) {
	s := NewStore()
	if err := s.EnablePersistence(filepath.Join(t.TempDir(), "timeline.json"), nil); err != nil {
		t.Fatal(err)
	}
	writes := 0
	s.writeFile = func(path string, data []byte, subject string) error {
		writes++
		if writes == 1 {
			return errors.New("injected historical write failure")
		}
		return writeFileAtomic(path, data, subject)
	}
	e := Event{IssueRef: "org/repo#1", Kind: KindMerged, At: 1234}
	s.RecordObservation(e)
	if !s.dirty {
		t.Fatal("failed precious observation was acknowledged")
	}
	s.RecordObservation(e)
	j, _ := s.Journey(e.IssueRef)
	if writes != 2 || s.dirty || j.Stages[KindMerged].Count != 1 {
		t.Fatalf("unchanged replay did not retry dirty persistence: writes=%d dirty=%v journey=%+v", writes, s.dirty, j)
	}
}

func TestSlowHistoricalPersistenceDoesNotHoldJourneyLockOrLoseConcurrentState(t *testing.T) {
	s := NewStore()
	path := filepath.Join(t.TempDir(), "timeline.json")
	if err := s.EnablePersistence(path, nil); err != nil {
		t.Fatal(err)
	}
	entered, release, recorded := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() { close(release); waitObservation(t, recorded) }()
	var writes atomic.Int32
	s.writeFile = func(path string, data []byte, subject string) error {
		if writes.Add(1) == 1 {
			close(entered)
			<-release
		}
		return writeFileAtomic(path, data, subject)
	}
	// Throttled non-precious current-work updates should not need a disk
	// barrier. Precious historical events still force their first write.
	s.lastPersist = time.Now()
	historical := Event{IssueRef: "org/repo#1", Kind: KindMerged, At: 1234}
	go func() { s.RecordObservation(historical); close(recorded) }()
	waitObservation(t, entered)
	current := Event{IssueRef: "org/repo#2", Kind: KindEnumerated, At: 5678}
	serviced := make(chan struct{})
	go func() {
		s.Record(current)
		_, _ = s.Journey(historical.IssueRef)
		close(serviced)
	}()
	waitObservation(t, serviced)
	// Releasing the writer happens in the defer, after proving a blocked
	// fsync/rename cannot hold the in-memory read/current-update mutex.
}

func TestRecordObservationDoesNotAcknowledgeConcurrentUnsavedState(t *testing.T) {
	s := NewStore()
	if err := s.EnablePersistence(filepath.Join(t.TempDir(), "timeline.json"), nil); err != nil {
		t.Fatal(err)
	}
	entered, release, recorded := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() { unblock(); waitObservation(t, recorded) }()
	s.writeFile = func(path string, data []byte, subject string) error {
		close(entered)
		<-release
		return writeFileAtomic(path, data, subject)
	}
	s.lastPersist = time.Now()
	historical := Event{IssueRef: "org/repo#1", Kind: KindMerged, At: 1234}
	go func() { s.RecordObservation(historical); close(recorded) }()
	waitObservation(t, entered)
	current := Event{IssueRef: "org/repo#2", Kind: KindEnumerated, At: 5678}
	updated := make(chan struct{})
	go func() { s.Record(current); close(updated) }()
	select {
	case <-updated:
	case <-time.After(observationTestTimeout):
		unblock()
		waitObservation(t, recorded)
		t.Fatal("current throttled state blocked on historical persistence")
	}
	unblock()
	waitObservation(t, recorded)
	if !s.dirty {
		t.Fatal("older historical snapshot acknowledged concurrent unsaved work")
	}
	s.writeFile = nil
	s.RecordObservation(historical)
	reloaded := NewStore()
	if err := reloaded.EnablePersistence(s.path, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Journey(current.IssueRef); !ok {
		t.Fatal("retry lost the concurrent current-work journey")
	}
}
