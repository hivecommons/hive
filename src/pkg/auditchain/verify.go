package auditchain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Problem is one place where a log fails verification.
type Problem struct {
	File   string `json:"file,omitempty"`
	Line   int    `json:"line"`
	Seq    int64  `json:"seq,omitempty"`
	Reason string `json:"reason"`
}

func (p Problem) String() string {
	loc := strconv.Itoa(p.Line)
	if p.File != "" {
		loc = p.File + ":" + loc
	}
	if p.Seq > 0 {
		return fmt.Sprintf("%s (seq %d): %s", loc, p.Seq, p.Reason)
	}
	return fmt.Sprintf("%s: %s", loc, p.Reason)
}

// Report is the outcome of verifying a log.
type Report struct {
	// Rows counts every non-blank line read.
	Rows int `json:"rows"`
	// Legacy counts rows written before the log was chained: unchained rows
	// that come before the first chained one. They are not covered by the
	// chain, and are not problems.
	Legacy int `json:"legacy"`
	// Chained counts rows that carry a row_hash.
	Chained int `json:"chained"`
	// FirstSeq is the seq of the oldest chained row read.
	FirstSeq int64 `json:"first_seq,omitempty"`
	// Anchor is the prev_hash of the oldest chained row read: "" when the
	// chain starts at its genesis row, otherwise the row_hash of a row that
	// is no longer in the files (rotated out by retention), which the
	// verifier takes on trust.
	Anchor string `json:"anchor,omitempty"`
	// Head is the newest chained row: the value to compare with the copy
	// reported to the hub.
	Head     Link      `json:"head"`
	Problems []Problem `json:"problems,omitempty"`
}

// OK reports whether the log verified without problems.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Verifier checks rows fed to it oldest first. The zero value is ready.
type Verifier struct {
	report  Report
	started bool
}

func (v *Verifier) problem(file string, line int, seq int64, format string, args ...any) {
	v.report.Problems = append(v.report.Problems, Problem{File: file, Line: line, Seq: seq, Reason: fmt.Sprintf(format, args...)})
}

// Add checks one row. file and line only label problems.
func (v *Verifier) Add(file string, line int, row []byte) {
	row = bytes.TrimSpace(row)
	if len(row) == 0 {
		return
	}
	v.report.Rows++
	obj, err := decodeObject(row)
	if err != nil {
		v.problem(file, line, 0, "malformed row: %v", err)
		return
	}
	stored, _ := obj[FieldRowHash].(string)
	if stored == "" {
		if !v.started {
			v.report.Legacy++
			return
		}
		v.problem(file, line, 0, "unchained row inside the chain (row inserted, or its chain fields stripped)")
		return
	}
	v.report.Chained++
	prevHash, _ := obj[FieldPrevHash].(string)
	var seq int64
	if n, ok := obj[FieldSeq].(json.Number); ok {
		seq, _ = n.Int64()
	}
	if seq <= 0 {
		v.problem(file, line, 0, "chained row has no valid seq")
	}
	if got, err := hashObject(obj); err != nil {
		v.problem(file, line, seq, "cannot hash row: %v", err)
	} else if got != stored {
		v.problem(file, line, seq, "row_hash mismatch: row was edited after it was written")
	}

	switch {
	case !v.started:
		v.started = true
		v.report.FirstSeq = seq
		v.report.Anchor = prevHash
		if prevHash == "" && seq != 1 {
			v.problem(file, line, seq, "chain starts without prev_hash at seq %d, not 1", seq)
		}
	case prevHash == "":
		v.problem(file, line, seq, "chain restarted after seq %d: the rows before were replaced or a new chain was begun", v.report.Head.Seq)
	case prevHash != v.report.Head.Hash:
		v.problem(file, line, seq, "prev_hash does not match the row before (seq %d): rows were removed, inserted or reordered", v.report.Head.Seq)
	case seq > 0 && seq != v.report.Head.Seq+1:
		v.problem(file, line, seq, "seq %d follows seq %d", seq, v.report.Head.Seq)
	}
	// Continue from the row as written, so one edited or dropped row is
	// reported once rather than cascading down the rest of the log.
	v.report.Head = Link{Seq: seq, Hash: stored}
}

// Report returns the result so far.
func (v *Verifier) Report() Report {
	r := v.report
	r.Problems = append([]Problem(nil), r.Problems...)
	return r
}

// VerifyLines verifies a whole log held in memory, oldest row first.
func VerifyLines(file string, data []byte) Report {
	var v Verifier
	for i, row := range bytes.Split(data, []byte("\n")) {
		v.Add(file, i+1, row)
	}
	return v.Report()
}
