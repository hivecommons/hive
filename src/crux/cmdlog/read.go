package cmdlog

import (
	"fmt"
	"sort"

	"github.com/hivecommons/hive/crux/api"
)

// Read folds runDir's log into command records and returns one page: the
// commands with a line after sinceSeq, ordered by their latest line, at
// most limit of them (0 takes api.CmdlogDefaultLimit; more than
// api.CmdlogMaxLimit is capped). A command whose started line was on an
// earlier page comes back once its ended line is written, so a caller that
// pages with NextSeq sees every command's final state. A torn last line is
// not yet committed and is left out. The page's Run is left for the caller.
func Read(runDir string, sinceSeq int64, limit int) (api.CommandLog, error) {
	switch {
	case limit <= 0:
		limit = api.CmdlogDefaultLimit
	case limit > api.CmdlogMaxLimit:
		limit = api.CmdlogMaxLimit
	}
	recs, err := fold(Path(runDir))
	if err != nil {
		return api.CommandLog{}, err
	}
	page := api.CommandLog{Commands: []api.CommandRecord{}, NextSeq: sinceSeq}
	var changed []api.CommandRecord
	for _, rec := range recs {
		if latest(rec) > sinceSeq {
			changed = append(changed, rec)
		}
	}
	sort.SliceStable(changed, func(i, j int) bool { return latest(changed[i]) < latest(changed[j]) })
	if len(changed) > limit {
		changed, page.More = changed[:limit], true
	}
	if n := len(changed); n > 0 {
		page.Commands, page.NextSeq = changed, latest(changed[n-1])
	}
	return page, nil
}

// latest is the sequence number of a record's last line.
func latest(rec api.CommandRecord) int64 {
	if rec.EndSeq > rec.Seq {
		return rec.EndSeq
	}
	return rec.Seq
}

// fold turns the committed lines into records in started order.
func fold(path string) ([]api.CommandRecord, error) {
	lines, _, err := readLines(path)
	if err != nil {
		return nil, err
	}
	var recs []api.CommandRecord
	index := map[int64]int{}
	for _, l := range lines {
		switch l.Event {
		case eventStarted:
			index[l.Seq] = len(recs)
			recs = append(recs, api.CommandRecord{
				Seq: l.Seq, Command: l.Command, Cwd: l.Cwd, Capture: l.Capture,
				Status: api.CommandUnfinished, StartedAt: l.At,
			})
		case eventEnded:
			i, ok := index[l.StartSeq]
			if !ok || recs[i].EndSeq != 0 {
				return nil, fmt.Errorf("%w: %s seq %d ends seq %d, which is not an open command", ErrCorrupt, path, l.Seq, l.StartSeq)
			}
			at := l.At
			rec := &recs[i]
			rec.EndSeq, rec.EndedAt, rec.Status = l.Seq, &at, l.Status
			rec.DurationMs, rec.ExitCode, rec.Reason = l.DurationMs, l.ExitCode, l.Reason
			rec.Stdout, rec.Stderr = l.Stdout, l.Stderr
		default:
			return nil, fmt.Errorf("%w: %s seq %d has unknown event %q", ErrCorrupt, path, l.Seq, l.Event)
		}
	}
	return recs, nil
}
