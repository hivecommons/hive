package config

// AdminMCPConfig is the `admin_mcp:` block: the owner's settings for this
// hive's HTTP admin MCP endpoint (/api/admin/mcp). The stdio hive-admin-mcp
// binary does not read it; that binary runs on an operator's machine and is
// configured entirely from its own environment.
type AdminMCPConfig struct {
	// WritesEnabled is the owner's switch for the endpoint's write tools
	// (write_preview / write_confirm), set from Settings → Channels.
	//
	// nil means the switch was never used, and HIVE_ADMIN_MCP_ENABLE_WRITES
	// on the hive process decides exactly as before: 1/true/yes/on turns
	// writes on, anything else (or unset) leaves them off. A non-nil value
	// overrides that environment variable in both directions, so the env is
	// only the default. The owner can also reset it to nil from the same tab
	// ("Use environment default"); omitempty then drops the key from every
	// persisted layer, so a restart does not bring the old value back. The
	// value is read on every MCP request, including write_confirm, so turning
	// writes off refuses pending confirmations at once.
	WritesEnabled *bool `yaml:"writes_enabled,omitempty" json:"writes_enabled,omitempty"`
}
