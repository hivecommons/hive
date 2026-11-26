package config

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/resolve"
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
//     that differs from the config file's is adopted, whole, when the config
//     file has not been edited since the overlay was saved (the overlay then
//     holds the latest edit; see overlayReconciledWith, which compares content
//     hashes, not mtimes) and the result still validates. Otherwise the config
//     file wins and the log names the block, so nothing is dropped without a
//     word.
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
	// base is the config file hash from the overlay's header line, nil when
	// the overlay has none (see parseOverlayBase).
	base *contentSum
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
//
// The overlay text is expanded with the CONFIG FILE's resolver registry
// (primaryData), never one built from the overlay's own variables block: the
// overlay is user-writable, and its resolver policy must not decide what
// runs while it is expanded (the security invariant at the end of
// loadWithDashboardOverlay).
func decodeDashboardOverlay(data, primaryData []byte) (*decodedOverlay, error) {
	var rawDoc, expDoc yaml.Node
	if err := yaml.Unmarshal(data, &rawDoc); err != nil {
		return nil, err
	}
	expanded := configRegistryFromText(string(primaryData)).Expand(context.Background(), string(data), resolve.ScopeConfig, nil)
	if err := yaml.Unmarshal([]byte(expanded), &expDoc); err != nil {
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
	if base, ok := parseOverlayBase(data); ok {
		out.base = &base
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

// Which layer holds the latest edit (hivecommons/hive#20066).
//
// #19740 first decided this by file mtime: an overlay block won only when the
// overlay file was newer than the config file. Every save writes the overlay
// first and the config file second, and on Kubernetes the boot chain (the
// copy-config init container, the entrypoint's copy of the runtime config)
// rewrites the config path, so the config file is newer on every boot and
// after every save, and the mtime said nothing about who edited what.
//
// Instead, every save records in the overlay's first line the content hash of
// the config file it writes in the same save. On load:
//
//   - the config file still has exactly that content: nobody changed it since
//     the overlay was written, so whatever differs in the overlay is the
//     newer edit (a hand edit of the overlay, #19740). Overlay blocks win.
//     A boot-time copy of the same content changes the mtime but not the
//     hash, so it does not count as an edit.
//   - the config file has other content: someone edited it after the overlay
//     was written (or the overlay was written by an older version, without
//     the header). The config file wins, a block missing there counts as
//     removed, and the next save writes it to the overlay.

// overlayBaseHeaderPrefix starts the overlay's first line. It is a YAML
// comment, so every reader of the overlay (the loader, the entrypoint's
// first-boot merge, backups) ignores it.
const overlayBaseHeaderPrefix = "# hive-overlay-base: sha256:"

// overlayBaseHeader is the first line saveDashboardOverlay writes: the hash of
// the config file content written by the same save.
func overlayBaseHeader(base contentSum) []byte {
	return []byte(fmt.Sprintf("%s%x\n", overlayBaseHeaderPrefix, base[:]))
}

// parseOverlayBase reads the config file hash from the overlay's first line.
// ok is false for an overlay without a well-formed header (written by an
// older version, or by hand from scratch).
func parseOverlayBase(data []byte) (base contentSum, ok bool) {
	line, _, _ := strings.Cut(string(data), "\n")
	hexSum, found := strings.CutPrefix(strings.TrimRight(line, "\r"), overlayBaseHeaderPrefix)
	if !found {
		return base, false
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(hexSum))
	if err != nil || len(decoded) != len(base) {
		return base, false
	}
	copy(base[:], decoded)
	return base, true
}

// overlayReconciledWith reports whether the config file content primaryData
// is the one the overlay was written against, i.e. the config file has not
// been edited since the overlay was saved.
func overlayReconciledWith(ov *decodedOverlay, primaryData []byte) bool {
	return ov.base != nil && *ov.base == sumBytes(primaryData)
}

// adoptOverlayBlocksGeneric carries every overlay block not covered by the
// dedicated adoptions onto cfg (see the file comment for the rule).
// primaryData is the config file content loadWithOverrides read; it is
// parsed without expansion, so no resolver runs a second time. Each adopted
// block is validated with the rest of the config and left out, loudly, when
// it does not validate, so an overlay edit can never make the hive persist a
// config file its next boot rejects. It reports whether anything was
// adopted.
func adoptOverlayBlocksGeneric(cfg *Config, path string, primaryData []byte, ov *decodedOverlay, opts ValidateOptions) bool {
	var primaryVals map[string]*yaml.Node
	var doc yaml.Node
	if yaml.Unmarshal(primaryData, &doc) == nil {
		if root, rerr := topLevelMapping(&doc); rerr == nil {
			primaryVals = mappingValues(root)
		}
	}
	overlayCurrent := overlayReconciledWith(ov, primaryData)
	var adopted []string
	for _, e := range ov.valid {
		key := e.key.Value
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
		if pv, inPrimary := primaryVals[key]; inPrimary {
			if po, perr := decodeEntry(e.key, pv); perr == nil && reflect.DeepEqual(po, oo) {
				continue
			}
		}
		content, _ := yaml.Marshal(e.expanded)
		if overlayHandledKeys[key] {
			if overlayCurrent {
				logOverlayOnce("handled:"+key, content, "[config] dashboard overlay %s block %q differs from %s; only the parts with their own reload rules are adopted from it (see loadWithDashboardOverlay), other changes in this block are not", DashboardOverlayFile, key, path)
			}
			continue
		}
		if why, owned := overlaySeedOwnedKeys[key]; owned {
			logOverlayOnce("owned:"+key, content, "[config] dashboard overlay %s block %q differs from %s and is not adopted: %s", DashboardOverlayFile, key, path, why)
			continue
		}
		if !overlayCurrent {
			// The config file changed since the overlay was written (or the
			// overlay predates the header), so it holds the latest edit,
			// including the removal of a block.
			logOverlayOnce("older:"+key, content, "[config] dashboard overlay %s block %q differs from %s, which changed after the overlay was saved (or the overlay has no hive-overlay-base line); keeping the config file's version (a block missing there counts as removed), and the next save writes it to the overlay", DashboardOverlayFile, key, path)
			continue
		}
		// The overlay block wins whole, the same rule as review and
		// agent_sandbox above: a key or map entry removed in the overlay
		// stays removed. Defaults are applied again by the caller.
		field := reflect.ValueOf(cfg).Elem().FieldByIndex(index)
		orig := reflect.New(field.Type()).Elem()
		orig.Set(field)
		field.Set(reflect.ValueOf(oo).FieldByIndex(index))
		// The overlay leaves dashboard.auth_token blank when it came from the
		// pod env or the token file (dashboardOverlayBytes), so a blank there
		// is not an edit: keep the token the config file loaded.
		if loaded, ok := orig.Interface().(DashboardConfig); ok && key == "dashboard" && cfg.Dashboard.AuthToken == "" {
			cfg.Dashboard.AuthToken = loaded.AuthToken
		}
		if err := cfg.validateWithOptions(opts); err != nil {
			field.Set(orig)
			logOverlayOnce("invalid:"+key, content, "[config] WARNING: dashboard overlay %s block %q is not adopted: it does not validate (%v); the config file's version is kept and the next save writes it to the overlay", DashboardOverlayFile, key, err)
			continue
		}
		adopted = append(adopted, key)
	}
	if len(adopted) == 0 {
		return false
	}
	sort.Strings(adopted)
	logOverlayOnce("adopted", []byte(strings.Join(adopted, ",")), "[config] reload: adopted dashboard overlay blocks %v (hivecommons/hive#19740)", adopted)
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
