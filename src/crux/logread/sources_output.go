package logread

import (
	"encoding/json"
	"strings"
	"time"
)

// sourceSpec is one entry of the fixed source list.
type sourceSpec struct {
	name string
	// stem names the files: <stem>.log and its rotated history.
	stem string
	// stamped sources hold outputRecord lines, whose text is returned as the
	// line and whose time and stream fill the line's fields.
	stamped bool
	// info reports the entry for log_sources, available or not with a reason.
	info func(opts Options) SourceInfo
	// dir is the directory the source's files live in.
	dir func(opts Options) string
}

const (
	proxySourceDescription      = "What the dashboard auth proxy prints, on standard output and standard error, kept with each line's time and stream. It still goes to the container's output as well."
	hiveOutputSourceDescription = "What the hive process prints before its own log is set up (for example a configuration that fails to load), kept with each line's time and stream. It still goes to the container's output as well."
	litellmSourceDescription    = "The local litellm proxy's standard output and standard error, kept with each line's time and stream. It is not added to the container's output."
	requestsSourceDescription   = "The dashboard's request log: time, method, path with query values removed, status, duration in milliseconds and authentication outcome (public, ok, failed or none). Terminal traffic the auth proxy handles itself and requests the gateway refuses never reach the dashboard and are not in it."
	startsSourceDescription     = "One line per hive start, newest last: the start's short identifier (hive log lines carry it as \"start\"), its start time, and how the previous run ended (clean, not-closed, self-upgrade-failed or unknown) with when it was last seen alive and its last memory and out-of-memory reading."
	crashSourceDescription      = "The Go runtime's trace of an unrecovered panic or fatal runtime error, one file per start, the last 10 kept, each at most 1 MiB. Each file opens with a line naming its start; anything after that line is the trace of how that process died. A process killed by SIGKILL or the out-of-memory killer leaves no trace."
)

var sourceSpecs = []sourceSpec{
	{
		name: SourceHive, stem: "hive",
		info: func(opts Options) SourceInfo { return hiveSourceInfo(opts.LogDir) },
		dir:  func(opts Options) string { return opts.LogDir },
	},
	{
		name: SourceProxy, stem: SourceProxy, stamped: true,
		info: func(opts Options) SourceInfo {
			if !opts.AuthProxyInstalled {
				return SourceInfo{Name: SourceProxy, Description: proxySourceDescription, Reason: "this image runs no dashboard auth proxy (for example the hub image)"}
			}
			return outputSourceInfo(SourceProxy, proxySourceDescription, opts)
		},
		dir: outputDirOf,
	},
	{
		name: SourceHiveOutput, stem: SourceHiveOutput, stamped: true,
		info: func(opts Options) SourceInfo {
			return outputSourceInfo(SourceHiveOutput, hiveOutputSourceDescription, opts)
		},
		dir: outputDirOf,
	},
	{
		name: SourceLiteLLM, stem: SourceLiteLLM, stamped: true,
		info: func(opts Options) SourceInfo {
			if !opts.LocalLiteLLM {
				return SourceInfo{Name: SourceLiteLLM, Description: litellmSourceDescription, Reason: "this hive does not run the local litellm proxy (governor.litellm.local_proxy is off)"}
			}
			return outputSourceInfo(SourceLiteLLM, litellmSourceDescription, opts)
		},
		dir: outputDirOf,
	},
	{
		name: SourceRequests, stem: SourceRequests,
		info: func(opts Options) SourceInfo {
			return outputSourceInfo(SourceRequests, requestsSourceDescription, opts)
		},
		dir: outputDirOf,
	},
	{
		name: SourceStarts, stem: SourceStarts,
		info: func(opts Options) SourceInfo {
			return outputSourceInfo(SourceStarts, startsSourceDescription, opts)
		},
		dir: outputDirOf,
	},
	{
		name: SourceCrash, stem: SourceCrash,
		info: func(opts Options) SourceInfo {
			return outputSourceInfo(SourceCrash, crashSourceDescription, opts)
		},
		dir: outputDirOf,
	},
}

func outputDirOf(opts Options) string { return opts.OutputDir }

// outputSourceInfo reports an output source whose process runs on this hive.
// It is available before its first line is written; a read then returns no
// lines.
func outputSourceInfo(name, description string, opts Options) SourceInfo {
	info := SourceInfo{Name: name, Description: description}
	if strings.TrimSpace(opts.OutputDir) == "" {
		info.Reason = "no output log directory is configured"
		return info
	}
	info.Available = true
	return info
}

func lookupSource(name string) (sourceSpec, bool) {
	for _, spec := range sourceSpecs {
		if spec.name == name {
			return spec, true
		}
	}
	return sourceSpec{}, false
}

// lineHead is what a line's record says about it besides its text.
type lineHead struct {
	when   time.Time
	level  string
	stream string
}

// lineHeads parses each line's time, level and stream. For a stamped source
// it replaces each record with its text, so scrubbing, filters and the result
// all see the text the process printed.
func lineHeads(lines []string, stamped bool) []lineHead {
	out := make([]lineHead, len(lines))
	for i, raw := range lines {
		if stamped {
			if head, text, ok := parseStamped(raw); ok {
				out[i], lines[i] = head, text
				continue
			}
		}
		out[i].when, out[i].level = parseHead(raw)
	}
	return out
}

// parseStamped decodes one outputRecord. The level, when there is one, comes
// from the printed text itself: the hive's early output is slog JSON.
func parseStamped(raw string) (lineHead, string, bool) {
	var rec struct {
		Time   string  `json:"time"`
		Stream string  `json:"stream"`
		Text   *string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.Text == nil {
		return lineHead{}, "", false
	}
	when, err := time.Parse(time.RFC3339Nano, rec.Time)
	if err != nil {
		return lineHead{}, "", false
	}
	head := lineHead{when: when}
	if rec.Stream == StreamStdout || rec.Stream == StreamStderr {
		head.stream = rec.Stream
	}
	_, head.level = parseHead(*rec.Text)
	return head, *rec.Text, true
}
