package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/forgejwt"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
)

// forgejoRESTSubject is the sub claim of the hive's own Forgejo REST JWT. The
// operator's REST integration matches it with --claim-eq sub=hive:rest, so a
// tier's git token is refused there and this token is refused by every tier's
// integration.
const forgejoRESTSubject = forgejwt.SubjectPrefix + "rest"

// forgejoRESTRefreshMargin is how long before expiry a cached REST JWT is
// minted again, so a client built just before expiry still has time to work.
const forgejoRESTRefreshMargin = 10 * time.Minute

// forgejoRESTSource is the token source reported for a minted REST JWT.
const forgejoRESTSource = "forgejo.integration.rest_audience"

// forgejoRESTNow is the clock for minting and refresh; tests replace it.
var forgejoRESTNow = time.Now

type forgejoRESTKey struct{ issuer, keyFile, audience string }

type forgejoRESTEntry struct {
	token string
	exp   time.Time
}

var forgejoRESTCache = struct {
	mu sync.Mutex
	m  map[forgejoRESTKey]forgejoRESTEntry
}{m: map[forgejoRESTKey]forgejoRESTEntry{}}

// forgejoRESTToken is the token the hive's Forgejo REST adapters send. With
// forgejo.integration.rest_audience set it is a one-hour JWT for that
// integration, reused until forgejoRESTRefreshMargin before it expires; with
// no rest_audience it is the forgejo block's access token, as before
// (hivecommons/hive#19920).
func forgejoRESTToken(cfg *config.Config) (token, source string, err error) {
	in := cfg.Forgejo.Integration
	aud := ""
	if in != nil {
		aud = strings.TrimSpace(in.RESTAudience)
	}
	if aud == "" {
		return cfg.Forgejo.Credentials().Governor().Resolve()
	}
	key := forgejoRESTKey{issuer: in.IssuerOrigin(), keyFile: strings.TrimSpace(in.KeyFile), audience: aud}
	now := forgejoRESTNow()

	forgejoRESTCache.mu.Lock()
	defer forgejoRESTCache.mu.Unlock()
	if e, ok := forgejoRESTCache.m[key]; ok && now.Add(forgejoRESTRefreshMargin).Before(e.exp) {
		return e.token, forgejoRESTSource, nil
	}
	signer := forgejwt.Signer{Issuer: forgejwt.Issuer(key.issuer), KeyFile: key.keyFile, Now: func() time.Time { return now }}
	tok, err := signer.Mint(forgejwt.Audience(aud), forgejoRESTSubject)
	if err != nil {
		return "", forgejoRESTSource, fmt.Errorf("%s: %w", forgejoRESTSource, err)
	}
	forgejoRESTCache.m[key] = forgejoRESTEntry{token: tok, exp: now.Add(forgejwt.Lifetime)}
	return tok, forgejoRESTSource, nil
}

// resolveForgeRESTToken is the REST token for a forge block: the Forgejo one
// goes through forgejoRESTToken, the others resolve gov as before.
func resolveForgeRESTToken(cfg *config.Config, kind forge.Kind, gov cruxconfig.TokenSource) (token, source string, err error) {
	if kind == forge.KindForgejo {
		return forgejoRESTToken(cfg)
	}
	return gov.Resolve()
}
