package inbox

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Deleting an Inbox item (hivecommons/hive#18694). Every kind of item can
// be deleted; what goes with it depends on where the item lives:
//
//   - hive-recording, upload, email: this hive stores the file, so the
//     stored audio or file, its extracted text and its machine transcript
//     record are removed.
//   - meeting (a meeting hive attended): the Inbox item only. The meeting
//     record, with its transcript and notes, stays in the Meetings section
//     and is deleted there.
//   - every synced source (plaud, zoom, teams, ...): the Inbox item only.
//     The recording stays with its source, and the Knowledge fact stays
//     because the connector sync owns it and would write it again.
//
// The Knowledge fact of an item this hive wrote itself (a recording, an
// upload) is kept unless DeleteOptions.RemoveKnowledge is set. Every delete
// leaves a tombstone in the state store, so a source that still lists the
// item (the next sync) never brings it back. What was already shared from
// the item (RFCs, issues, DIBS ideas, spek seeds) is not touched.

var (
	// ErrItemNotFound is returned for an item the Inbox does not list.
	ErrItemNotFound = errors.New("no such inbox item")
	// ErrItemTranscribing refuses a delete while a transcription is queued
	// or running for the item.
	ErrItemTranscribing = errors.New("this item is being transcribed; delete it once the transcription finishes")
	// ErrItemOwnerOnly refuses a non-owner's delete of a mailed-in item
	// that still waits for the owner's approval.
	ErrItemOwnerOnly = errors.New("only the owner decides on a mailed-in item that waits for approval")
)

// DeleteOptions says who deletes and what goes with the item.
type DeleteOptions struct {
	// Actor is the dashboard user; AsOwner is true for the hive owner.
	Actor   string
	AsOwner bool
	// RemoveKnowledge also deletes the item's Knowledge fact when this
	// hive wrote it (KnowledgeRemovable). The default keeps it.
	RemoveKnowledge bool
	// Recording audits a hive recording's delete like DeleteRecording.
	Recording RecordingOptions
}

// ItemDeleted is what DeleteItem did to one item.
type ItemDeleted struct {
	Source string `json:"source"`
	ItemID string `json:"item_id"`
	// StoredRemoved reports that the file, recording or audio this hive
	// stored for the item was deleted.
	StoredRemoved bool `json:"stored_removed"`
	// KnowledgeRemoved and KnowledgeKept say what happened to the item's
	// Knowledge fact; both are false when it had none.
	KnowledgeRemoved bool `json:"knowledge_removed"`
	KnowledgeKept    bool `json:"knowledge_kept"`
	// SharesKept is how many shares the item's history held.
	SharesKept int `json:"shares_kept"`
}

// StoredLocally reports whether this hive itself stores the file behind an
// item of source (so a delete removes it).
func StoredLocally(source string) bool {
	return source == RecordingType || source == UploadType || source == EmailType
}

// KnowledgeRemovable reports whether a delete may also remove the item's
// Knowledge fact: only a fact this hive wrote for its own item. A synced
// source's fact belongs to its connector, which would write it again.
func KnowledgeRemovable(it Item) bool {
	return it.FactPath != "" && (it.Source == RecordingType || it.Source == UploadType)
}

// Live returns items without the ones deleted from the Inbox.
func (s *Store) Live(items []Item) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	out := items[:0]
	for _, it := range items {
		if e := s.entries[it.Key()]; e != nil && e.DeletedAt != nil {
			continue
		}
		out = append(out, it)
	}
	return out
}

// Deleted reports whether key carries a delete tombstone.
func (s *Store) Deleted(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	e := s.entries[key]
	return e != nil && e.DeletedAt != nil
}

// Tombstone drops every persisted state of one item (routes, intents,
// share history) and leaves a delete marker in its place, so the item is
// never listed again. It reports how many shares the history held.
func (s *Store) Tombstone(key, by string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	n := 0
	if e := s.entries[key]; e != nil {
		n = len(e.History)
	}
	now := s.now()
	s.entries[key] = &entryState{DeletedAt: &now, DeletedBy: by}
	return n, s.save()
}

// DeleteItem deletes one item from the Inbox for good (see the top of this
// file for what goes with it). A hive recording keeps DeleteRecording's
// rule: its recorder or an owner. An item whose transcription is queued or
// running is refused (ErrItemTranscribing).
func (s *Service) DeleteItem(ctx context.Context, source, id string, opts DeleteOptions) (ItemDeleted, error) {
	out := ItemDeleted{Source: source, ItemID: id}
	it, found, _ := s.Find(ctx, source, id)
	if !found {
		return out, ErrItemNotFound
	}
	if it.AwaitingApproval && !opts.AsOwner {
		return out, ErrItemOwnerOnly
	}
	removeFact := opts.RemoveKnowledge && KnowledgeRemovable(it)
	var errs []error
	if source == RecordingType {
		d, err := s.deleteRecording(id, opts.Actor, opts.AsOwner, opts.Recording, !removeFact)
		if d.ID == "" {
			if errors.Is(err, ErrRecordingTranscribing) {
				err = ErrItemTranscribing
			}
			return out, err
		}
		out.StoredRemoved, out.KnowledgeRemoved, out.SharesKept = true, d.FactRemoved, d.SharesKept
		if err != nil {
			errs = append(errs, err)
		}
	} else {
		if err := s.deleteStored(it, removeFact, &out); err != nil {
			if out.StoredRemoved || !StoredLocally(source) {
				errs = append(errs, err)
			} else {
				return out, err
			}
		}
	}
	out.KnowledgeKept = it.FactSlug != "" && !out.KnowledgeRemoved
	n, err := s.Store.Tombstone(it.Key(), opts.Actor)
	if err != nil {
		errs = append(errs, fmt.Errorf("record the delete: %w", err))
	}
	if n > out.SharesKept {
		out.SharesKept = n
	}
	return out, errors.Join(errs...)
}

// deleteStored removes what this hive holds for a non-recording item: the
// stored upload or mailed-in file, the machine transcript record and, when
// asked, the Knowledge fact. It holds the transcription lock so no job is
// queued for the item between the busy check and the delete.
func (s *Service) deleteStored(it Item, removeFact bool, out *ItemDeleted) error {
	store := s.transcriptStore()
	s.txMu.Lock()
	defer s.txMu.Unlock()
	if store != nil {
		if rec, ok := store.Get(it.Source, it.ID); ok && (rec.Status == TranscriptionQueued || rec.Status == TranscriptionRunning) {
			return ErrItemTranscribing
		}
	}
	switch it.Source {
	case UploadType:
		u := s.Uploads()
		if u == nil {
			return ErrUploadsDisabled
		}
		if err := u.Store.Delete(it.ID); err != nil {
			return fmt.Errorf("delete the upload: %w", err)
		}
		out.StoredRemoved = true
	case EmailType:
		e := s.Email()
		if e == nil {
			return ErrEmailNotFound
		}
		if err := e.Decline(it.ID); err != nil {
			return fmt.Errorf("delete the mailed-in file: %w", err)
		}
		out.StoredRemoved = true
	}
	var errs []error
	if removeFact {
		s.factMu.Lock()
		err := os.Remove(it.FactPath)
		s.factMu.Unlock()
		switch {
		case err == nil:
			out.KnowledgeRemoved = true
		case !os.IsNotExist(err):
			errs = append(errs, fmt.Errorf("delete the Knowledge fact: %w", err))
		}
	}
	if store != nil {
		if err := store.Delete(it.Source, it.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete the transcript record: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Delete removes one upload: its original file, its extracted text and its
// metadata. The Knowledge fact written for it is not touched.
func (s *UploadStore) Delete(id string) error {
	dir, err := s.dir(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := readMeta(dir); err != nil {
		return ErrUploadNotFound
	}
	return os.RemoveAll(dir)
}
