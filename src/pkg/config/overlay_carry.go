package config

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Generic dashboard-overlay carry (hivecommons/hive#19740).
//
// loadWithDashboardOverlay used to adopt a fixed list of blocks from the
// dashboard overlay (otel/tracing, governor.work_source, operator cadences,
// review, agent_sandbox, repos.*.open_prs, removed_agents, agents). Any other
// block an operator edited into the overlay by hand (ask, chat, customers, …)
// was dropped by the reload, and because the reload also recorded the
// overlay's content hash, the next persist rewrote the overlay and the config
// file from memory without the edit and without a word in the log. #5632,
// #18804 and the open_prs fix closed that one block at a time.
//
// This file closes it for every block:
//
//   - Reload: every top-level overlay key the schema knows is carried, except
//     the keys dedicated code above already adopts (overlayHandledKeys) and
//     the keys the overlay must never override (overlaySeedOwnedKeys). A block
//     is adopted when the config file has no such key (there is nothing to
//     lose), or when the overlay file is newer than the config file (it holds
//     the latest edit). When the config file is newer and says something
//     different, the config file wins and the log says so.
//   - Persist: an overlay key the schema does not know, or one that fails to
//     decode, is kept verbatim (pre-expansion text) and written back into the
//     overlay on every save instead of being erased. An overlay that is not
//     valid YAML at all is not overwritten until it is fixed.
//
// Nothing here ever reads overlay.Variables (see the security invariant at
// the end of loadWithDashboardOverlay).

// overlayHandledKeys are adopted by dedicated code in loadWithDashboardOverlay
// with their own rules (whole agents behind the fullness guard, only the
// operator-owned cadences of governor, only open_prs of each repo, …). The
// generic carry leaves them to that code.
var overlayHandledKeys = map[string]bool{
	"agents":         true,
	"removed_agents": true,
	"otel":           true,
	"tracing":        true,
	"governor":       true,
	"review":         true,
	"agent_sandbox":  true,
	"repos":          true,
	// project: adoptOverlayRepoOpenPRs takes only open_prs from each repo
	// policy, so the config file's companion repos and the watched-repo
	// filter stay authoritative. Carrying the whole block would undo that.
	"project": true,
}

// overlaySeedOwnedKeys are never taken from the overlay. The value is the
// reason, logged when the overlay disagrees with the config file so the
// operator knows the edit did not take effect.
var overlaySeedOwnedKeys = map[string]string{
	"variables":  "the resolver trust policy is read from the config file only (security invariant)",
	"github":     "GitHub App credentials follow the entrypoint's placeholder rules at boot",
	"hub":        "hub settings are managed by the hub",
	"acmm_level": "the ACMM level is set by the hub and the pack apply",
	"hive_id":    "the hive id is assigned at provisioning",
}

// preservedOverlayBlock is one top-level overlay entry the in-memory config
// cannot represent (unknown key, or a value that does not decode). value is
// the pre-expansion node, so no expanded secret is ever written back.
type preservedOverlayBlock struct {
	key   string
	value *yaml.Node
}

// overlayCarryState is what a Config loaded with the dashboard overlay must
// put back when it saves the overlay.
type overlayCarryState struct {
	// preserved blocks are written into the overlay on every save, replacing
	// whatever the marshaled config has under the same key.
	preserved []preservedOverlayBlock
	// keepWhole is the content of an overlay that is not valid YAML. A save
	// leaves an overlay with exactly this content alone.
	keepWhole *contentSum
}

var (
	topLevelKeysOnce sync.Once
	topLevelKeys     map[string][]int
)

// configTopLevelKeys returns every top-level hive.yaml key the Config schema
// decodes, with the field index path that holds it, following
// `yaml:",inline"` embeds the way yaml.v3 does.
func configTopLevelKeys() map[string][]int {
	topLevelKeysOnce.Do(func() {
		topLevelKeys = map[string][]int{}
		collectYAMLKeys(reflect.TypeOf(Config{}), nil, topLevelKeys)
	})
	return topLevelKeys
}

func collectYAMLKeys(t reflect.Type, prefix []int, into map[string][]int) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue // unexported
		}
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		index := append(append([]int(nil), prefix...), i)
		name, flags, _ := strings.Cut(tag, ",")
		if strings.Contains(","+flags+",", ",inline,") {
			if f.Type.Kind() == reflect.Struct {
				collectYAMLKeys(f.Type, index, into)
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		into[name] = index
	}
}

// overlayEntry is one top-level overlay key with its expanded (decoded) and
// raw (written back) value nodes.
type overlayEntry struct {
	key      *yaml.Node
	expanded *yaml.Node
	raw      *yaml.Node
}

// decodedOverlay is the dashboard overlay split into what the Config can
// hold and what it cannot.
type decodedOverlay struct {
	cfg Config
	// valid lists the known keys that decoded, in file order.
	valid []overlayEntry
	carry *overlayCarryState
}

// topLevelMapping returns the top-level mapping of a parsed document, nil for
// an empty document, and an error when the document is not a mapping.
func topLevelMapping(doc *yaml.Node) (*yaml.Node, error) {
	if doc.Kind == 0 {
		return nil, nil // empty file
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 0 {
		return nil, nil // comments only
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("not a YAML document")
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return nil, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("top level is not a mapping")
	}
	return root, nil
}

func mappingValues(m *yaml.Node) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	if m == nil {
		return out
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		out[m.Content[i].Value] = m.Content[i+1]
	}
	return out
}

// decodeEntry decodes a single top-level key into a fresh Config.
func decodeEntry(key, value *yaml.Node) (Config, error) {
	var probe Config
	single := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{key, value}}
	err := single.Decode(&probe)
	return probe, err
}

// decodeDashboardOverlay parses the overlay file content key by key. A key
// that is unknown or does not decode is logged and preserved; it no longer
// throws away the rest of the overlay. err is non-nil only when the file is
// not a YAML mapping at all.
func decodeDashboardOverlay(data []byte) (*decodedOverlay, error) {
	var rawDoc, expDoc yaml.Node
	if err := yaml.Unmarshal(data, &rawDoc); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal([]byte(expandEnvVars(string(data))), &expDoc); err != nil {
		return nil, err
	}
	rawRoot, err := topLevelMapping(&rawDoc)
	if err != nil {
		return nil, err
	}
	expRoot, err := topLevelMapping(&expDoc)
	if err != nil {
		return nil, err
	}
	rawVals := mappingValues(rawRoot)
	known := configTopLevelKeys()
	out := &decodedOverlay{carry: &overlayCarryState{}}
	validMap := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if expRoot != nil {
		for i := 0; i+1 < len(expRoot.Content); i += 2 {
			k, v := expRoot.Content[i], expRoot.Content[i+1]
			raw := rawVals[k.Value]
			if _, ok := known[k.Value]; !ok {
				preserveOverlayBlock(out.carry, k.Value, raw, "it is not a key this hive version knows")
				continue
			}
			if _, derr := decodeEntry(k, v); derr != nil {
				preserveOverlayBlock(out.carry, k.Value, raw, "it does not parse: "+derr.Error())
				continue
			}
			validMap.Content = append(validMap.Content, k, v)
			out.valid = append(out.valid, overlayEntry{key: k, expanded: v, raw: raw})
		}
	}
	if err := validMap.Decode(&out.cfg); err != nil {
		return nil, err
	}
	if len(out.carry.preserved) == 0 {
		out.carry = nil
	}
	return out, nil
}

func preserveOverlayBlock(carry *overlayCarryState, key string, raw *yaml.Node, why string) {
	if raw == nil {
		// Only possible when env expansion created the key; there is no raw
		// text to keep, and writing the expanded text could leak a secret.
		logOverlayOnce("unkept:"+key, nil, "[config] WARNING: dashboard overlay %s block %q is not loaded (%s) and cannot be kept on save", DashboardOverlayFile, key, why)
		return
	}
	carry.preserved = append(carry.preserved, preservedOverlayBlock{key: key, value: raw})
	b, _ := yaml.Marshal(raw)
	logOverlayOnce("kept:"+key, b, "[config] WARNING: dashboard overlay %s block %q is not loaded (%s); it is kept as written and saves will not erase it", DashboardOverlayFile, key, why)
}

// overlayWarned dedupes the loud overlay warnings per key and content, so a
// block that stays broken is reported once rather than on every reload.
var (
	overlayWarnedMu sync.Mutex
	overlayWarned   = map[string]contentSum{}
)

func logOverlayOnce(id string, content []byte, format string, args ...any) {
	sum := sumBytes(content)
	overlayWarnedMu.Lock()
	last, seen := overlayWarned[id]
	overlayWarned[id] = sum
	overlayWarnedMu.Unlock()
	if seen && last == sum {
		return
	}
	log.Printf(format, args...)
}

// fileNewer reports whether a's modification time is strictly after b's.
func fileNewer(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return ai.ModTime().After(bi.ModTime())
}

// adoptOverlayBlocksGeneric carries every overlay block not covered by the
// dedicated adoptions onto cfg (see the file comment for the rule). It
// reports whether anything was adopted.
func adoptOverlayBlocksGeneric(cfg *Config, path string, ov *decodedOverlay) bool {
	var primaryVals map[string]*yaml.Node
	if data, err := os.ReadFile(path); err == nil {
		var doc yaml.Node
		if yaml.Unmarshal([]byte(expandEnvVars(string(data))), &doc) == nil {
			if root, rerr := topLevelMapping(&doc); rerr == nil {
				primaryVals = mappingValues(root)
			}
		}
	}
	overlayNewer := fileNewer(DashboardOverlayFile, path)
	var adopted []string
	for _, e := range ov.valid {
		key := e.key.Value
		if overlayHandledKeys[key] {
			continue
		}
		index, ok := configTopLevelKeys()[key]
		if !ok {
			continue
		}
		oo, oerr := decodeEntry(e.key, e.expanded)
		if oerr != nil || reflect.ValueOf(oo).FieldByIndex(index).IsZero() {
			// An empty block (a whole-Config marshal writes non-omitempty
			// blocks even when they are zero) says nothing; adopting it
			// would only blank the loaded block's values.
			continue
		}
		pv, inPrimary := primaryVals[key]
		if inPrimary {
			if po, perr := decodeEntry(e.key, pv); perr == nil && reflect.DeepEqual(po, oo) {
				continue
			}
		}
		if why, owned := overlaySeedOwnedKeys[key]; owned {
			b, _ := yaml.Marshal(e.expanded)
			logOverlayOnce("owned:"+key, b, "[config] dashboard overlay %s block %q differs from %s and is not adopted: %s", DashboardOverlayFile, key, path, why)
			continue
		}
		if inPrimary && !overlayNewer {
			b, _ := yaml.Marshal(e.expanded)
			logOverlayOnce("older:"+key, b, "[config] dashboard overlay %s block %q differs from %s, which is newer; keeping the config file's block (the next save writes it to the overlay)", DashboardOverlayFile, key, path)
			continue
		}
		// Decoded onto the loaded block, not into a fresh one: keys the
		// overlay block leaves out keep the config file's value (and any
		// default), the way a partial hand edit such as `ask: {enabled:
		// false}` means it. Config's own UnmarshalYAML replaces the whole
		// struct, so the field is addressed directly.
		field := reflect.ValueOf(cfg).Elem().FieldByIndex(index)
		if err := e.expanded.Decode(field.Addr().Interface()); err != nil {
			// decodeDashboardOverlay already decoded this entry, so this is
			// not expected; say so rather than drop it quietly.
			log.Printf("[config] WARNING: dashboard overlay %s block %q could not be applied: %v", DashboardOverlayFile, key, err)
			continue
		}
		adopted = append(adopted, key)
	}
	if len(adopted) == 0 {
		return false
	}
	sort.Strings(adopted)
	log.Printf("[config] reload: adopted dashboard overlay blocks %v (hivecommons/hive#19740)", adopted)
	return true
}

// spliceOverlayBlocks writes the preserved blocks into marshaled overlay
// bytes, replacing a key the config marshaled under the same name and
// appending the rest.
func spliceOverlayBlocks(data []byte, blocks []preservedOverlayBlock) ([]byte, error) {
	if len(blocks) == 0 {
		return data, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	root, err := topLevelMapping(&doc)
	if err != nil {
		return nil, err
	}
	if root == nil {
		root = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	}
	for _, b := range blocks {
		replaced := false
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == b.key {
				root.Content[i+1] = b.value
				replaced = true
				break
			}
		}
		if !replaced {
			root.Content = append(root.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: b.key}, b.value)
		}
	}
	return yaml.Marshal(&doc)
}

// overlayKeptWhole reports whether the overlay on disk is the unparseable
// content this Config was loaded with, which a save must leave alone.
func (c *Config) overlayKeptWhole() bool {
	if c.overlayCarry == nil || c.overlayCarry.keepWhole == nil {
		return false
	}
	cur, ok := sumFile(DashboardOverlayFile)
	return ok && cur == *c.overlayCarry.keepWhole
}
