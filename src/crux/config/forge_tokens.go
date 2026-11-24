package config

import "os"

// TokenSource names where one static forge token lives. It never holds the
// secret.
type TokenSource struct {
	TokenEnv string `yaml:"token_env,omitempty"`
}

// ForgeTokens is the credential half of every forge block (gitlab, gitea,
// forgejo), embedded inline so each block reads its token the same way.
type ForgeTokens struct {
	TokenSource `yaml:",inline"`
}

// ForgeCredentials binds a block's tokens to its default env var.
type ForgeCredentials struct {
	DefaultEnv string
	Tokens     ForgeTokens
}

// Governor returns the source of the hive's own forge token. An unset env name
// becomes the block default, so TokenEnv always names the variable to strip from
// agent sessions.
func (c ForgeCredentials) Governor() TokenSource {
	s := c.Tokens.TokenSource
	if s.TokenEnv == "" {
		s.TokenEnv = c.DefaultEnv
	}
	return s
}

// EnvNames returns the env var names that carry this block's credential, for
// the agent-session strip registry.
func (c ForgeCredentials) EnvNames() []string {
	return []string{c.Governor().TokenEnv}
}

// Resolve returns the token and where it came from. An unset env var is not an
// error: the token is empty and the forge answers 401 with a message that names
// the real problem.
func (s TokenSource) Resolve() (token, source string) {
	return os.Getenv(s.TokenEnv), KeySourceEnvPrefix + s.TokenEnv
}
