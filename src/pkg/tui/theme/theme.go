// Package theme is the TUI's palette: every color the frame draws, named by the ROLE
// it plays rather than by the color it happens to be, and given twice — once
// for a light terminal background and once for a dark one.
//
// WHY ADAPTIVE (T25, #5139). A single value cannot serve both backgrounds. The
// focus border shipped as ANSI 205 (#ff5fd7), chosen against a dark terminal
// where it is a ~7.9:1 contrast highlight that unmistakably says "this pane
// has focus". The same pink on a white background is ~2.4:1 — barely a tint —
// so an operator on a light terminal got a focus indicator they could not see.
// That is not a taste question, it is the one thing the border is for.
//
// WHY A PACKAGE rather than loose vars in tui. A token has exactly one
// definition and every call site names it, so the palette can be read in one
// place and a later change lands once. It is also the seam configurable themes
// (explicitly out of scope here) would plug into: swap the values here, not
// the call sites.
//
// SCOPE. The tokens below are the roles with call sites in the frame and the
// overlays. They are named for meaning (accent, warning, selected) rather than
// hue so a later theme can retint the TUI without changing callers.
//
// WHERE THIS LIVES. A leaf package under pkg/tui, imported by both pkg/tui and
// pkg/tui/panes. It began life in package tui, on the reasoning that panes/
// rendered no color at all and so needed no access; the note there said the
// first pane that genuinely needed a token should move the file to a leaf
// package, "a rename and two import lines". The help overlay (T23, #5155) is
// that pane — it borders its box in the frame's emphasis color — and pkg/tui
// imports pkg/tui/panes, so reaching back up would be an import cycle. This is
// that move, made when a caller required it rather than in anticipation.
package theme

import "github.com/charmbracelet/lipgloss"

// The values use the terminal's first 16 ANSI slots wherever possible. Those
// slots inherit an operator's terminal theme, unlike fixed xterm-256 cube
// indices, and therefore have a better chance of staying legible on terminals
// this project has never seen.
//
// The pairs are matched by CONTRAST AGAINST THEIR OWN BACKGROUND, not by
// looking similar to each other, because that is what makes the frame read the
// same way in both:
//
//   - Border: black/bright-black remain recessive chrome on the background
//     they are paired with.
//   - BorderFocus and Accent: magenta is the frame's emphasis color.
//   - Success/Warning/Danger use the conventional green/yellow/red state
//     slots, reinforced by words and glyphs so color is never the only signal.
//
// Under termenv's Ascii profile — which is what `go test` renders through —
// both halves resolve to no color at all, so the golden files are unaffected
// by this change and by which background a machine reports.
var (
	// Text is normal foreground content.
	Text = lipgloss.AdaptiveColor{Light: "0", Dark: "15"}

	// Muted is secondary text and rules.
	Muted = lipgloss.AdaptiveColor{Light: "8", Dark: "7"}

	// Accent is the active/emphasized foreground.
	Accent = lipgloss.AdaptiveColor{Light: "5", Dark: "13"}

	// Success marks healthy affirmative state.
	Success = lipgloss.AdaptiveColor{Light: "2", Dark: "10"}

	// Warning marks pending or cautionary state.
	Warning = lipgloss.AdaptiveColor{Light: "3", Dark: "11"}

	// Danger marks destructive actions and failed state.
	Danger = lipgloss.AdaptiveColor{Light: "1", Dark: "9"}

	// Selected is the selected row background.
	Selected = lipgloss.AdaptiveColor{Light: "7", Dark: "8"}

	// Border is the frame's de-emphasized chrome: the border around every
	// pane that does not have focus.
	Border = lipgloss.AdaptiveColor{Light: "8", Dark: "7"}

	// BorderFocus is the border around the focused pane, and the border of
	// the help overlay's box. It is the frame's only emphasis color, so it
	// must read as clearly emphasized on either background — see the
	// contrast note above.
	BorderFocus = lipgloss.AdaptiveColor{Light: "5", Dark: "13"}
)
