package theme

func Catalog() []Theme {
	return []Theme{
		openclaw(), openclawLight(), honeycomb(), graphite(), nord(), dracula(), solarizedDark(), githubLight(), highContrast(),
	}
}

func baseDark() map[string]string {
	return map[string]string{
		"--bg": "#080b0f", "--bg-soft": "#0d1218", "--panel": "#121922", "--panel-strong": "#17212d", "--text": "#f6f8fb", "--muted": "#a8b3c2", "--line": "#263545", "--line-strong": "#263545", "--amber": "#f4c75f", "--green": "#74df9a", "--blue": "#80bfff", "--red": "#ff7e7e", "--yellow": "#d29922", "--orange": "#e38b2c", "--indigo": "#818cf8", "--cyan": "#7bd8cd", "--purple": "#c0a6f0", "--radius-sm": "4px", "--radius": "6px", "--radius-lg": "12px", "--shadow-modal": "0 20px 60px rgba(0,0,0,.5)", "--shadow-raised": "0 4px 16px rgba(0,0,0,.45)", "--font-ui": "Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif", "--font-mono": "'SF Mono', 'Cascadia Code', 'Fira Code', monospace", "--fs-xs": ".68rem", "--fs-sm": ".75rem", "--fs-base": ".82rem", "--fs-md": ".92rem", "--fs-lg": "1.05rem", "--fs-xl": "1.25rem", "--terminal-bg": "#05070a", "--terminal-line": "#2a3442", "--terminal-fg": "#d7dfe9", "--terminal-muted": "#8b98a9", "--terminal-cyan": "#7bd8cd", "--sidebar-w": "302px", "--surface": "var(--panel)", "--border": "var(--line)", "--fg": "var(--text)", "--card-bg": "var(--panel)", "--accent": "var(--blue)", "--oc-accent": "var(--red)", "--oc-accent-light": "rgba(255,126,126,.12)", "--green-bg": "rgba(116,223,154,.15)", "--green-border": "rgba(116,223,154,.35)", "--red-bg": "rgba(255,126,126,.15)", "--red-border": "rgba(255,126,126,.35)",
	}
}

func baseLight() map[string]string {
	m := baseDark()
	m["--bg"] = "#f7f8fa"
	m["--bg-soft"] = "#ffffff"
	m["--panel"] = "#ffffff"
	m["--panel-strong"] = "#eef1f5"
	m["--text"] = "#111827"
	m["--muted"] = "#6b7280"
	m["--line"] = "#e5e7eb"
	m["--line-strong"] = "#c6cdd6"
	m["--amber"] = "#b45309"
	m["--green"] = "#16a34a"
	m["--blue"] = "#2563eb"
	m["--red"] = "#dc2626"
	m["--yellow"] = "#a16207"
	m["--orange"] = "#c2570f"
	m["--indigo"] = "#4f46e5"
	m["--cyan"] = "#0891b2"
	m["--purple"] = "#7c3aed"
	m["--shadow-modal"] = "0 20px 60px rgba(0,0,0,.15)"
	m["--shadow-raised"] = "0 4px 16px rgba(0,0,0,.12)"
	m["--accent"] = "var(--red)"
	m["--oc-accent-light"] = "rgba(220,38,38,.08)"
	m["--green-bg"] = "#f0fdf4"
	m["--green-border"] = "#bbf7d0"
	m["--red-bg"] = "#fef2f2"
	m["--red-border"] = "#fecaca"
	m["--terminal-bg"] = "#1d2430"
	return m
}

func openclaw() Theme {
	return Theme{ID: "openclaw", Name: "OpenClaw", Description: "Current dark dashboard palette with blue mode accent", Dark: true, Tokens: baseDark(), Fonts: ThemeFonts{UI: "Inter, ui-sans-serif, system-ui", Mono: "'SF Mono', 'Cascadia Code', monospace"}}
}
func openclawLight() Theme {
	return Theme{ID: "openclaw-light", Name: "OpenClaw Light", Description: "Current light dashboard palette with red mode accent", Dark: false, Tokens: baseLight(), Fonts: ThemeFonts{UI: "Inter, ui-sans-serif, system-ui", Mono: "'SF Mono', 'Cascadia Code', monospace"}}
}
func with(base map[string]string, pairs map[string]string) map[string]string {
	for k, v := range pairs {
		base[k] = v
	}
	return base
}

func honeycomb() Theme {
	return Theme{ID: "honeycomb", Name: "Honeycomb", Description: "Hivecommons honey on wax with a subtle comb watermark", Dark: true, Tokens: with(baseDark(), map[string]string{"--bg": "#14110b", "--bg-soft": "#19140c", "--panel": "#21190e", "--panel-strong": "#2a2113", "--text": "#fff6df", "--muted": "#c7a96b", "--line": "#3f321d", "--line-strong": "#5a441f", "--amber": "#e0a33a", "--accent": "#e0a33a", "--blue": "#e0a33a", "--red": "#f26d5b", "--green": "#9fd27a", "--font-ui": "'IBM Plex Sans', Inter, ui-sans-serif, system-ui", "--font-mono": "'IBM Plex Mono', 'SF Mono', monospace"}), Fonts: ThemeFonts{UI: "'IBM Plex Sans', Inter, ui-sans-serif, system-ui", Mono: "'IBM Plex Mono', 'SF Mono', monospace"}, Background: &ThemeBackground{Image: HoneycombDataURI, Position: "top right", Size: "56px 97px", Opacity: .22, Attachment: "fixed"}}
}
func graphite() Theme {
	return Theme{ID: "graphite", Name: "Graphite", Description: "Neutral monochrome dashboard chrome", Dark: true, Tokens: with(baseDark(), map[string]string{"--bg": "#0b0c0e", "--bg-soft": "#111317", "--panel": "#181b20", "--panel-strong": "#22262d", "--text": "#f2f4f8", "--muted": "#a5adba", "--line": "#323843", "--line-strong": "#454c59", "--amber": "#d0d5dd", "--accent": "#d0d5dd", "--blue": "#9ca3af", "--red": "#ef7373", "--green": "#8bd49c"}), Fonts: ThemeFonts{UI: "Inter, ui-sans-serif, system-ui", Mono: "'SF Mono', monospace"}}
}
func nord() Theme {
	return Theme{ID: "nord", Name: "Nord", Description: "Arctic north-bluish clean theme", Dark: true, Tokens: with(baseDark(), map[string]string{"--bg": "#2e3440", "--bg-soft": "#343b49", "--panel": "#3b4252", "--panel-strong": "#434c5e", "--text": "#eceff4", "--muted": "#a8b3c2", "--line": "#4c566a", "--line-strong": "#5e81ac", "--amber": "#ebcb8b", "--accent": "#88c0d0", "--blue": "#5e81ac", "--red": "#bf616a", "--green": "#a3be8c", "--yellow": "#ebcb8b", "--cyan": "#8fbcbb", "--purple": "#b48ead", "--font-ui": "'IBM Plex Sans', -apple-system, sans-serif", "--font-mono": "'IBM Plex Mono', monospace"}), Fonts: ThemeFonts{UI: "'IBM Plex Sans', -apple-system, sans-serif", Mono: "'IBM Plex Mono', monospace"}}
}
func dracula() Theme {
	return Theme{ID: "dracula", Name: "Dracula", Description: "Dark purple vampire theme", Dark: true, Tokens: with(baseDark(), map[string]string{"--bg": "#282a36", "--bg-soft": "#2f3242", "--panel": "#343746", "--panel-strong": "#44475a", "--text": "#f8f8f2", "--muted": "#a6accd", "--line": "#4b4f63", "--line-strong": "#6272a4", "--amber": "#f1fa8c", "--accent": "#bd93f9", "--blue": "#8be9fd", "--red": "#ff5555", "--green": "#50fa7b", "--yellow": "#f1fa8c", "--orange": "#ffb86c", "--cyan": "#8be9fd", "--purple": "#bd93f9", "--font-ui": "'Fira Sans', -apple-system, sans-serif", "--font-mono": "'Fira Code', monospace"}), Fonts: ThemeFonts{UI: "'Fira Sans', -apple-system, sans-serif", Mono: "'Fira Code', monospace"}}
}
func solarizedDark() Theme {
	return Theme{ID: "solarized-dark", Name: "Solarized Dark", Description: "Precision colors for machines and people", Dark: true, Tokens: with(baseDark(), map[string]string{"--bg": "#002b36", "--bg-soft": "#073642", "--panel": "#073642", "--panel-strong": "#0b3f4b", "--text": "#93a1a1", "--muted": "#839496", "--line": "#164e5a", "--line-strong": "#586e75", "--amber": "#b58900", "--accent": "#2aa198", "--blue": "#268bd2", "--red": "#dc322f", "--green": "#859900", "--yellow": "#b58900", "--orange": "#cb4b16", "--cyan": "#2aa198", "--purple": "#6c71c4", "--font-ui": "'Source Sans Pro', -apple-system, sans-serif", "--font-mono": "'Source Code Pro', monospace"}), Fonts: ThemeFonts{UI: "'Source Sans Pro', -apple-system, sans-serif", Mono: "'Source Code Pro', monospace"}}
}
func githubLight() Theme {
	return Theme{ID: "github-light", Name: "GitHub Light", Description: "Clean and professional", Dark: false, Tokens: with(baseLight(), map[string]string{"--bg": "#ffffff", "--bg-soft": "#f6f8fa", "--panel": "#ffffff", "--panel-strong": "#f6f8fa", "--text": "#24292f", "--muted": "#57606a", "--line": "#d0d7de", "--line-strong": "#afb8c1", "--amber": "#bf8700", "--accent": "#0969da", "--blue": "#0969da", "--red": "#cf222e", "--green": "#1f883d", "--yellow": "#9a6700", "--purple": "#8250df", "--font-ui": "-apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif", "--font-mono": "'SFMono-Regular', Consolas, monospace"}), Fonts: ThemeFonts{UI: "-apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif", Mono: "'SFMono-Regular', Consolas, monospace"}}
}
func highContrast() Theme {
	return Theme{ID: "high-contrast", Name: "High Contrast", Description: "WCAG AAA high-contrast theme without translucency", Dark: true, Tokens: with(baseDark(), map[string]string{"--bg": "#000000", "--bg-soft": "#050505", "--panel": "#0c0c0c", "--panel-strong": "#181818", "--text": "#ffffff", "--muted": "#e5e5e5", "--line": "#ffffff", "--line-strong": "#ffffff", "--amber": "#ffd700", "--accent": "#00ffff", "--blue": "#00ffff", "--red": "#ff5c5c", "--green": "#00ff66", "--yellow": "#ffff00", "--orange": "#ff9900", "--cyan": "#00ffff", "--purple": "#ff7cff", "--shadow-modal": "0 0 0 2px #ffffff", "--shadow-raised": "0 0 0 1px #ffffff", "--oc-accent-light": "#220000", "--green-bg": "#002b10", "--green-border": "#00ff66", "--red-bg": "#2b0000", "--red-border": "#ff5c5c"}), Fonts: ThemeFonts{UI: "Inter, ui-sans-serif, system-ui", Mono: "'SF Mono', monospace"}}
}
