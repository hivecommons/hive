package config

import (
	"fmt"
	"strings"
)

// project.remote_write_identity: which GitHub identity a hive's writes to a
// write-tier reference repo appear under.
//
// A reference repo (#18015) is one the hive does not manage; its write tier
// comes only from a grant (a hand-written entry, or one derived from an
// accepted request issue, #18139). This setting never widens that grant. It
// only picks whose name is on the issues, PRs and comments the grant allows:
//
//   - RemoteWriteIdentityBot (the default, and what an empty value means):
//     the hive's GitHub App, as "<app>[bot]", on a granted repo whose owner
//     has installed that App on their own; the hive never asks anyone to
//     install it. Where the App is not installed, the hive owner's connected
//     account is used instead, and with no connected account the entry stays
//     read-only, exactly as before this setting existed.
//   - RemoteWriteIdentityBotOnly: the App bot where it is installed, and
//     read-only everywhere else. The owner's connected account is never
//     used, even when it is connected.
//   - RemoteWriteIdentityOwner: the hive owner's connected account on every
//     write-tier reference repo (the behaviour #18197 introduced).
//
// A hand-written entry's write_token_file always wins over all three.
const (
	RemoteWriteIdentityBot     = "bot"
	RemoteWriteIdentityBotOnly = "bot_only"
	RemoteWriteIdentityOwner   = "owner"
)

// RemoteWriteIdentities is the vocabulary, in the order the Settings control
// offers it.
var RemoteWriteIdentities = []string{RemoteWriteIdentityBot, RemoteWriteIdentityBotOnly, RemoteWriteIdentityOwner}

// NormalizeRemoteWriteIdentity folds case and whitespace, accepts "-" for
// "_", and maps the empty value to the default. An unknown value is returned
// folded so the validator can name it.
func NormalizeRemoteWriteIdentity(v string) string {
	v = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(v)), "-", "_")
	if v == "" {
		return RemoteWriteIdentityBot
	}
	return v
}

// ValidateRemoteWriteIdentity accepts "" and the RemoteWriteIdentities.
func ValidateRemoteWriteIdentity(v string) error {
	switch NormalizeRemoteWriteIdentity(v) {
	case RemoteWriteIdentityBot, RemoteWriteIdentityBotOnly, RemoteWriteIdentityOwner:
		return nil
	}
	return fmt.Errorf("project.remote_write_identity: %q is not one of %q, %q, %q", strings.TrimSpace(v), RemoteWriteIdentityBot, RemoteWriteIdentityBotOnly, RemoteWriteIdentityOwner)
}

// EffectiveRemoteWriteIdentity is the setting with the default applied.
// Loading validates the value; an unknown one that slipped past (a hand
// edit picked up mid-reload) reads as bot_only, the identity that can never
// put the owner's name on anything.
func (p ProjectConfig) EffectiveRemoteWriteIdentity() string {
	switch v := NormalizeRemoteWriteIdentity(p.RemoteWriteIdentity); v {
	case RemoteWriteIdentityBot, RemoteWriteIdentityBotOnly, RemoteWriteIdentityOwner:
		return v
	}
	return RemoteWriteIdentityBotOnly
}

// Credential identities a write-tier reference repo resolves to
// (ReferenceRepo.CredentialIdentity).
const (
	// ReferenceIdentityBot: writes appear as the hive's GitHub App bot,
	// through a token minted on the App's installation covering the repo,
	// scoped to that one repo.
	ReferenceIdentityBot = "bot"
	// ReferenceIdentityOwner: writes appear as the hive owner's connected
	// GitHub account.
	ReferenceIdentityOwner = "owner"
	// ReferenceIdentityTokenFile: writes appear as the user of a hand-written
	// entry's write_token_file.
	ReferenceIdentityTokenFile = "token_file"
)
