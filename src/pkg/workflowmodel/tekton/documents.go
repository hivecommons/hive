package tekton

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tekton API groups whose documents the ingest considers.
var tektonAPIGroups = []string{"tekton.dev/", "triggers.tekton.dev/", "pipelinesascode.tekton.dev/"}

// Resource kinds, as they appear in the kind field.
const (
	kindPipeline        = "Pipeline"
	kindPipelineRun     = "PipelineRun"
	kindTask            = "Task"
	kindTaskRun         = "TaskRun"
	kindClusterTask     = "ClusterTask"
	kindStepAction      = "StepAction"
	kindEventListener   = "EventListener"
	kindTriggerTemplate = "TriggerTemplate"
	kindTriggerBinding  = "TriggerBinding"
	kindTrigger         = "Trigger"
	kindList            = "List"
)

var kindLinePattern = regexp.MustCompile(`(?m)^kind:\s*(Pipeline|PipelineRun|Task|TaskRun|ClusterTask|StepAction|EventListener|TriggerTemplate|TriggerBinding|Trigger)\s*$`)

// LooksTekton is a cheap pre-filter on raw bytes: the data mentions a
// tekton.dev API group or has a kind line naming a Tekton resource.
func LooksTekton(data []byte) bool {
	return bytes.Contains(data, []byte("tekton.dev/")) || kindLinePattern.Match(data)
}

// IsTektonDocument reports whether a parsed YAML mapping has an apiVersion
// in the tekton.dev, triggers.tekton.dev or pipelinesascode.tekton.dev
// groups.
func IsTektonDocument(doc map[string]any) bool {
	apiVersion := stringAt(doc, "apiVersion")
	for _, g := range tektonAPIGroups {
		if strings.HasPrefix(apiVersion, g) {
			return true
		}
	}
	return false
}

// SplitDocuments decodes every YAML document in data, skipping empty ones
// and expanding kind: List into its items. Documents that are not mappings
// are dropped. It stops at the first malformed document and returns the
// error.
func SplitDocuments(data []byte) ([]map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []map[string]any
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		m, ok := doc.(map[string]any)
		if !ok {
			continue
		}
		if stringAt(m, "kind") == kindList {
			for _, item := range sliceAt(m, "items") {
				if im, ok := item.(map[string]any); ok {
					out = append(out, im)
				}
			}
			continue
		}
		out = append(out, m)
	}
}

// stringAt returns m[key] when it is a string, else "".
func stringAt(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// mapAt returns m[key] when it is a mapping, else nil.
func mapAt(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	sub, _ := m[key].(map[string]any)
	return sub
}

// sliceAt returns m[key] when it is a sequence, else nil.
func sliceAt(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	s, _ := m[key].([]any)
	return s
}

// mapsAt returns the mapping elements of the sequence at m[key].
func mapsAt(m map[string]any, key string) []map[string]any {
	items := sliceAt(m, key)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if im, ok := it.(map[string]any); ok {
			out = append(out, im)
		}
	}
	return out
}

// stringsAt returns m[key] as strings: a scalar becomes a one-element
// slice, a sequence keeps its scalar elements.
func stringsAt(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, it := range v {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// metadataName returns metadata.name, falling back to generateName with
// its trailing hyphen removed.
func metadataName(doc map[string]any) string {
	md := mapAt(doc, "metadata")
	if name := stringAt(md, "name"); name != "" {
		return name
	}
	return strings.TrimSuffix(stringAt(md, "generateName"), "-")
}

// walkStrings calls fn with every string scalar reachable from v.
func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case []any:
		for _, it := range t {
			walkStrings(it, fn)
		}
	case map[string]any:
		for _, it := range t {
			walkStrings(it, fn)
		}
	}
}
