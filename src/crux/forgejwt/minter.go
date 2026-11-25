package forgejwt

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/crux/pushbroker"
)

// SubjectPrefix starts the sub claim; the tier follows it ("hive:advisor").
// The operator's integration for a tier pins it with --claim-eq sub=hive:<tier>,
// so a token minted for one tier is refused by every other tier's
// integration even if the audiences were swapped.
const SubjectPrefix = "hive:"

// Subject is the sub claim for tier.
func Subject(tier agentmode.TokenTier) string { return SubjectPrefix + string(tier) }

// Minter is the Forgejo analogue of the GitHub App installation-token
// minter: a pushbroker.TieredMinter whose tokens are one-hour JWTs for the
// authorized integration made for the agent's token tier. Each push and
// each clone mints a fresh one.
type Minter struct {
	Signer Signer
	// Audiences maps a token tier to the aud of the integration made for it.
	Audiences map[agentmode.TokenTier]Audience
	// Tier is the tier this minter mints for; empty is
	// pushbroker.DefaultTier.
	Tier agentmode.TokenTier
}

var _ pushbroker.TieredMinter = Minter{}

// WithTier implements pushbroker.TieredMinter.
func (m Minter) WithTier(tier agentmode.TokenTier) pushbroker.TokenMinter {
	m.Tier = tier
	return m
}

// MintPushToken implements pushbroker.TokenMinter. The repo is not a claim:
// Forgejo limits a token to the repos its integration was created for.
func (m Minter) MintPushToken(_ context.Context, _ string) (string, error) {
	tier, aud, err := m.audience()
	if err != nil {
		return "", err
	}
	return m.Signer.Mint(aud, Subject(tier))
}

// audience returns the tier the token is minted for and its audience: the
// minter's own tier when it has one, else the closest less-privileged tier
// that does. It never moves to a more-privileged tier, and it refuses an
// unknown tier.
func (m Minter) audience() (agentmode.TokenTier, Audience, error) {
	tier := agentmode.TokenTier(strings.TrimSpace(string(m.Tier)))
	if tier == "" {
		tier = pushbroker.DefaultTier
	}
	for t, ok := tier, true; ok; t, ok = agentmode.LessPrivilegedTokenTier(t) {
		if aud := Audience(strings.TrimSpace(string(m.Audiences[t]))); aud != "" {
			return t, aud, nil
		}
	}
	if len(m.Audiences) == 0 {
		return "", "", errors.New("forgejwt: no integration audiences are configured")
	}
	return "", "", fmt.Errorf("forgejwt: no Forgejo integration is configured for token tier %q or a less-privileged one", tier)
}
