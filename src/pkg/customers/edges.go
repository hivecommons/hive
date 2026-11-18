package customers

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Edge predicates.
const (
	PredAuthored    = "authored"
	PredAssigned    = "assigned"
	PredReviewed    = "reviewed"
	PredMerged      = "merged"
	PredClaimed     = "claimed"
	PredReported    = "reported"
	PredCommentedOn = "commented_on"
	PredMentionedIn = "mentioned_in"
	PredSent        = "sent"
	PredAdded       = "added"
	PredRecorded    = "recorded"
	PredShared      = "shared"
	PredOrganised   = "organised"
	PredAttended    = "attended"
	PredSpokeIn     = "spoke_in"
	PredChattedIn   = "chatted_in"
	PredInvited     = "invited"
	PredDecided     = "decided"
	PredCitedIn     = "cited_in"
	// PredActed and PredLoggedIn are stored as one edge per person per day
	// (the artefact is an AuditRef).
	PredActed    = "acted"
	PredLoggedIn = "logged_in"
)

const (
	// IndexFileName is the edge index inside the graph data directory.
	IndexFileName = "customers.db"
	indexDirMode  = 0o700
	indexFileMode = 0o600
	// indexOpenTimeout bounds the wait for the file lock, so a second
	// opener fails instead of hanging.
	indexOpenTimeout = 5 * time.Second
	// maxRefLen bounds an artefact reference.
	maxRefLen = 512
	// maxPredicateLen bounds a predicate name.
	maxPredicateLen = 40
	// MaxEdgePage is the largest page of edges one read returns.
	MaxEdgePage = 100
	keyDelim    = "\x00"
)

var (
	// bucketPerson holds person\0reverse-time\0predicate\0artefact -> edge
	// JSON, so a person's edges read newest first.
	bucketPerson = []byte("pe")
	// bucketArtefact holds artefact\0person\0predicate -> the person key,
	// for the reverse walk and for de-duplication.
	bucketArtefact = []byte("ae")
)

// Edge ties a person to an artefact. It stores a reference, never content.
type Edge struct {
	Person    string `json:"person"`
	Predicate string `json:"predicate"`
	Artefact  string `json:"artefact"`
	// Via is the key of the identity that produced the edge. It makes a
	// split exact: the edges an identity produced move with it.
	Via        string    `json:"via,omitempty"`
	Source     string    `json:"source,omitempty"`
	At         time.Time `json:"at"`
	Confidence float64   `json:"confidence,omitempty"`
}

// Index is the edge index: a bbolt file that can be rebuilt from the
// sources at any time. It records no human decision.
type Index struct {
	db *bolt.DB
}

// OpenIndex opens or creates <dir>/customers.db.
func OpenIndex(dir string) (*Index, error) {
	if err := os.MkdirAll(dir, indexDirMode); err != nil {
		return nil, fmt.Errorf("customers: creating index dir: %w", err)
	}
	db, err := bolt.Open(filepath.Join(dir, IndexFileName), indexFileMode, &bolt.Options{Timeout: indexOpenTimeout})
	if err != nil {
		return nil, fmt.Errorf("customers: opening index: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketPerson, bucketArtefact} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("customers: preparing index: %w", err)
	}
	return &Index{db: db}, nil
}

// Close releases the index file.
func (x *Index) Close() error { return x.db.Close() }

// reverseTime sorts newest first as a fixed-width decimal.
func reverseTime(t time.Time) string {
	return fmt.Sprintf("%020d", uint64(math.MaxInt64)-uint64(t.UnixNano()))
}

func personKey(e Edge) []byte {
	return []byte(e.Person + keyDelim + reverseTime(e.At) + keyDelim + e.Predicate + keyDelim + e.Artefact)
}

func artefactKey(e Edge) []byte {
	return []byte(e.Artefact + keyDelim + e.Person + keyDelim + e.Predicate)
}

func validEdge(e Edge) bool {
	if e.Person == "" || e.Predicate == "" || len(e.Predicate) > maxPredicateLen || !validRef(e.Artefact) {
		return false
	}
	for _, s := range []string{e.Person, e.Predicate} {
		for _, r := range s {
			if r < ' ' || r == 0x7f {
				return false
			}
		}
	}
	return !e.At.IsZero() && e.At.UnixNano() >= 0
}

func putEdge(tx *bolt.Tx, e Edge) error {
	pe, ae := tx.Bucket(bucketPerson), tx.Bucket(bucketArtefact)
	e.At = e.At.UTC()
	pk, ak := personKey(e), artefactKey(e)
	// One edge per (person, predicate, artefact): a re-sighting with a new
	// time replaces the old row.
	if old := ae.Get(ak); old != nil && !bytes.Equal(old, pk) {
		if err := pe.Delete(old); err != nil {
			return err
		}
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := pe.Put(pk, data); err != nil {
		return err
	}
	return ae.Put(ak, pk)
}

func deleteEdge(tx *bolt.Tx, e Edge) error {
	if err := tx.Bucket(bucketPerson).Delete(personKey(e)); err != nil {
		return err
	}
	return tx.Bucket(bucketArtefact).Delete(artefactKey(e))
}

// Put writes edges. An edge that cannot be stored (no person, no time, a
// malformed reference) is skipped. It returns how many were written.
func (x *Index) Put(edges []Edge) (int, error) {
	n := 0
	err := x.db.Update(func(tx *bolt.Tx) error {
		for _, e := range edges {
			if !validEdge(e) {
				continue
			}
			if err := putEdge(tx, e); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("customers: writing edges: %w", err)
	}
	return n, nil
}

// PutNew writes the edges the index does not hold yet, keyed by (person,
// predicate, artefact), and leaves an existing edge exactly as it is, so a
// weaker source cannot overwrite the time or source of a stronger one. An
// edge that cannot be stored is skipped. It returns how many were written.
func (x *Index) PutNew(edges []Edge) (int, error) {
	n := 0
	err := x.db.Update(func(tx *bolt.Tx) error {
		ae := tx.Bucket(bucketArtefact)
		for _, e := range edges {
			if !validEdge(e) {
				continue
			}
			e.At = e.At.UTC()
			if ae.Get(artefactKey(e)) != nil {
				continue
			}
			if err := putEdge(tx, e); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("customers: writing edges: %w", err)
	}
	return n, nil
}

// ByPerson returns a page of a person's edges, newest first. predicate
// filters when not empty. after is the cursor a previous page returned;
// next is empty on the last page.
func (x *Index) ByPerson(person, predicate, after string, limit int) (edges []Edge, next string, err error) {
	if limit <= 0 || limit > MaxEdgePage {
		limit = MaxEdgePage
	}
	prefix := []byte(person + keyDelim)
	var start []byte
	if after != "" {
		start, err = hex.DecodeString(after)
		if err != nil || !bytes.HasPrefix(start, prefix) {
			return nil, "", fmt.Errorf("%w: bad cursor", ErrInvalid)
		}
	}
	err = x.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketPerson).Cursor()
		var k, v []byte
		if start != nil {
			k, v = c.Seek(start)
			if k != nil && bytes.Equal(k, start) {
				k, v = c.Next()
			}
		} else {
			k, v = c.Seek(prefix)
		}
		for ; k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var e Edge
			if json.Unmarshal(v, &e) != nil {
				continue
			}
			if predicate != "" && e.Predicate != predicate {
				continue
			}
			if len(edges) == limit {
				next = hex.EncodeToString(personKey(edges[len(edges)-1]))
				return nil
			}
			edges = append(edges, e)
		}
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("customers: reading edges: %w", err)
	}
	return edges, next, nil
}

// ByArtefact returns every edge that points at an artefact.
func (x *Index) ByArtefact(artefact string) ([]Edge, error) {
	var edges []Edge
	prefix := []byte(artefact + keyDelim)
	err := x.db.View(func(tx *bolt.Tx) error {
		pe := tx.Bucket(bucketPerson)
		c := tx.Bucket(bucketArtefact).Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var e Edge
			if data := pe.Get(v); data != nil && json.Unmarshal(data, &e) == nil {
				edges = append(edges, e)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("customers: reading edges: %w", err)
	}
	return edges, nil
}

// personEdges reads every edge of a person inside a transaction.
func personEdges(tx *bolt.Tx, person string) []Edge {
	var edges []Edge
	prefix := []byte(person + keyDelim)
	c := tx.Bucket(bucketPerson).Cursor()
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		var e Edge
		if json.Unmarshal(v, &e) == nil {
			edges = append(edges, e)
		}
	}
	return edges
}

// Counts returns how many edges a person has, by predicate.
func (x *Index) Counts(person string) (map[string]int, error) {
	counts := map[string]int{}
	err := x.db.View(func(tx *bolt.Tx) error {
		for _, e := range personEdges(tx, person) {
			counts[e.Predicate]++
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("customers: reading edges: %w", err)
	}
	return counts, nil
}

// DeletePerson removes every edge of a person (forget). It returns how many
// were removed.
func (x *Index) DeletePerson(person string) (int, error) {
	return x.DeleteWhere(person, func(Edge) bool { return true })
}

// DeleteWhere removes the edges of a person that match. It returns how many
// were removed.
func (x *Index) DeleteWhere(person string, match func(Edge) bool) (int, error) {
	n := 0
	err := x.db.Update(func(tx *bolt.Tx) error {
		for _, e := range personEdges(tx, person) {
			if !match(e) {
				continue
			}
			if err := deleteEdge(tx, e); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("customers: deleting edges: %w", err)
	}
	return n, nil
}

// Move re-homes the edges of one person that match onto another (merge,
// unmerge, split). It returns how many moved.
func (x *Index) Move(from, to string, match func(Edge) bool) (int, error) {
	if from == to || to == "" {
		return 0, fmt.Errorf("%w: bad move", ErrInvalid)
	}
	n := 0
	err := x.db.Update(func(tx *bolt.Tx) error {
		for _, e := range personEdges(tx, from) {
			if !match(e) {
				continue
			}
			if err := deleteEdge(tx, e); err != nil {
				return err
			}
			e.Person = to
			if err := putEdge(tx, e); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("customers: moving edges: %w", err)
	}
	return n, nil
}

// ViaAny matches the edges produced by any of the identity keys.
func ViaAny(keys []string) func(Edge) bool {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return func(e Edge) bool { return set[e.Via] }
}

// Prune removes every edge whose artefact no longer exists, so a profile
// cannot keep what retention deleted. It returns how many were removed.
func (x *Index) Prune(exists func(artefact string) bool) (int, error) {
	n := 0
	err := x.db.Update(func(tx *bolt.Tx) error {
		pe, ae := tx.Bucket(bucketPerson), tx.Bucket(bucketArtefact)
		verdict := map[string]bool{}
		var gonePerson, goneArtefact [][]byte
		c := ae.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			ref, _, _ := bytes.Cut(k, []byte(keyDelim))
			keep, seen := verdict[string(ref)]
			if !seen {
				keep = exists(string(ref))
				verdict[string(ref)] = keep
			}
			if keep {
				continue
			}
			goneArtefact = append(goneArtefact, append([]byte(nil), k...))
			gonePerson = append(gonePerson, append([]byte(nil), v...))
		}
		for i := range goneArtefact {
			if err := ae.Delete(goneArtefact[i]); err != nil {
				return err
			}
			if err := pe.Delete(gonePerson[i]); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("customers: pruning edges: %w", err)
	}
	return n, nil
}

// Len is the number of edges in the index.
func (x *Index) Len() int {
	n := 0
	_ = x.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketPerson).Stats().KeyN
		return nil
	})
	return n
}
