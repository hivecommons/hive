package askai

import _ "embed"

// The panel component, shared by the hub and hive dashboards so both show the
// same consent notice, label and fallback. Served from 'self' only.
var (
	//go:embed web/ask-panel.js
	PanelJS []byte
	//go:embed web/ask-panel.css
	PanelCSS []byte
)
