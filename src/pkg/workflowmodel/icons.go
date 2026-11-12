package workflowmodel

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ColumnIconIDs is the allowlist of icon ids a Column.Icon may name. The
// SPA draws each id from its own inline-SVG set (STAGE_ICON_SET in
// static-src/js/tracker-stage-icons.js; a dashboard test keeps the two
// lists equal), so a model can never inject markup into a chevron
// (hivecommons/hive#18033). Order is the editor's pick-list order.
var ColumnIconIDs = []string{
	"inbox", "circle", "bolt", "hand", "person", "pencil", "pull-request",
	"flask", "gear", "eye", "people", "thumbs-up", "shield-check", "merge",
	"check", "checkbox", "list", "tag", "flag", "rocket", "book", "search",
	"clock", "pause", "archive", "dot",
}

// ColumnIconDefault is the icon an unknown column name falls back to.
const ColumnIconDefault = "dot"

// columnIconMaxEmojiRunes bounds an emoji override: one pictograph plus
// its variation selector, skin tone or ZWJ-joined companions.
const columnIconMaxEmojiRunes = 8

var columnIconIDSet = func() map[string]bool {
	m := make(map[string]bool, len(ColumnIconIDs))
	for _, id := range ColumnIconIDs {
		m[id] = true
	}
	return m
}()

// IsColumnIconID reports whether id is an allowlisted icon id.
func IsColumnIconID(id string) bool { return columnIconIDSet[id] }

// IsColumnIconEmoji reports whether s is a single emoji (one pictograph
// with optional modifiers and joiners): no letters, digits, spaces,
// punctuation or markup, so a user-typed override can only ever render as
// a glyph.
func IsColumnIconEmoji(s string) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > columnIconMaxEmojiRunes {
		return false
	}
	pictographs := 0
	for _, r := range s {
		switch {
		case columnIconJoiner(r):
		case columnIconPictograph(r):
			pictographs++
		default:
			return false
		}
	}
	return pictographs >= 1 && pictographs <= 2
}

// columnIconJoiner: ZWJ, variation selectors, keycap and skin tones — the
// code points that modify a pictograph without being one.
func columnIconJoiner(r rune) bool {
	return r == 0x200D || r == 0xFE0F || r == 0xFE0E || r == 0x20E3 || (r >= 0x1F3FB && r <= 0x1F3FF)
}

// columnIconPictograph covers the emoji blocks (Supplemental Symbols and
// Pictographs, Misc Symbols, Dingbats, Arrows, Geometric Shapes, …) and
// the regional indicators that pair into flags.
func columnIconPictograph(r rune) bool {
	if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) || unicode.IsControl(r) || r < 0x80 {
		return false
	}
	ranges := [][2]rune{{0x1F000, 0x1FAFF}, {0x2600, 0x27BF}, {0x2B00, 0x2BFF}, {0x2300, 0x23FF}, {0x2190, 0x21FF}, {0x25A0, 0x25FF}, {0x2900, 0x297F}, {0x1F1E6, 0x1F1FF}}
	for _, rg := range ranges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	switch r {
	case 0x00A9, 0x00AE, 0x2122, 0x2139, 0x3030, 0x303D, 0x3297, 0x3299:
		return true
	}
	return false
}

// ValidateColumnIcon accepts the empty value (default by name), an
// allowlisted icon id, or a single emoji; anything else is an error naming
// what was rejected.
func ValidateColumnIcon(icon string) error {
	v := strings.TrimSpace(icon)
	if v == "" || IsColumnIconID(v) || IsColumnIconEmoji(v) {
		return nil
	}
	return fmt.Errorf("icon %q is neither an icon id (%s) nor a single emoji", icon, strings.Join(ColumnIconIDs, ", "))
}
