package checkout

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// store is the record layout: Dir/<id>.json per checkout and Dir/index.jsonl
// as the append-only create/delete history.
type store struct{ dir string }

// indexEntry is one line of index.jsonl.
type indexEntry struct {
	Op     string    `json:"op"`
	ID     string    `json:"id"`
	Owner  string    `json:"owner"`
	Repo   string    `json:"repo"`
	Commit string    `json:"commit"`
	At     time.Time `json:"at"`
	By     string    `json:"by,omitempty"`
}

func (st *store) recordPath(id string) string { return filepath.Join(st.dir, id+".json") }

func (st *store) put(rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := st.recordPath(rec.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, recordMode); err != nil {
		return err
	}
	return os.Rename(tmp, st.recordPath(rec.ID))
}

func (st *store) get(id string) (Record, error) {
	if !ValidID(id) {
		return Record{}, fmt.Errorf("%w: %q is not a checkout id", ErrNotFound, id)
	}
	data, err := os.ReadFile(st.recordPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, fmt.Errorf("%w: checkout %s", ErrNotFound, id)
	}
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("checkout %s: corrupt record: %w", id, err)
	}
	return rec, nil
}

func (st *store) remove(id string) error {
	err := os.Remove(st.recordPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (st *store) list() ([]Record, error) {
	entries, err := os.ReadDir(st.dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Record{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !ValidID(id) {
			continue
		}
		rec, err := st.get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (st *store) appendIndex(entry indexEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(st.dir, IndexName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, recordMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
