package customers

import (
	"context"
	"errors"
	"strings"
)

// The accept-time profile fetch: one request for the public GitHub profile of
// a login a person has just accepted. It reads only the name, company and
// location the person published themselves, and writes them as automatic
// profile fields. It shares the accept-time search's gate and hourly budget
// (MentionSearch), so it is off exactly when that search is off, stands down
// under the same quota rules, and is never run by the discovery sweep.

// PublicProfile is a person's published GitHub profile text.
type PublicProfile struct {
	Name, Company, Location string
}

// ProfileSource makes one GitHub request for a login's public profile.
type ProfileSource interface {
	PublicProfile(ctx context.Context, login string) (PublicProfile, error)
}

// ProfileFetchResult counts what one fetch did: ids and counts only.
type ProfileFetchResult struct {
	Person  string `json:"person"`
	Skipped string `json:"skipped,omitempty"`
	Filled  int    `json:"filled"`
}

// NormalizeCompany drops the "@" GitHub profiles put before an organization
// handle ("@ibm" is shown as "ibm") and collapses whitespace.
func NormalizeCompany(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	return strings.Join(strings.Fields(s), " ")
}

// FetchProfile fetches the public profile of an accepted GitHub login and
// fills the person's automatic profile fields. A field a person set by hand
// is never changed.
func (m *MentionSearch) FetchProfile(ctx context.Context, person string, ident Identity) ProfileFetchResult {
	res := ProfileFetchResult{Person: person}
	switch {
	case m == nil || m.Profiles == nil || m.Store == nil:
		res.Skipped = SkipNoSearcher
		return res
	case m.Enabled == nil || !m.Enabled():
		res.Skipped = SkipDisabled
		return res
	case ident.Kind != KindGitHub || strings.TrimSpace(ident.Value) == "" || IsBotLogin(ident.Value):
		res.Skipped = SkipNotLogin
		return res
	}
	if m.QuotaLow != nil && m.QuotaLow() != "" {
		res.Skipped = SkipQuotaLow
		return res
	}
	limit := 0
	if m.HourlyLimit != nil {
		limit = m.HourlyLimit()
	}
	if m.reserve(1, limit) == 0 {
		res.Skipped = SkipHourlyBudget
		return res
	}
	prof, err := m.Profiles.PublicProfile(ctx, strings.TrimPrefix(strings.TrimSpace(ident.Value), "@"))
	if err != nil {
		if errors.Is(err, ErrMentionQuotaLow) {
			res.Skipped = SkipQuotaLow
		} else {
			res.Skipped = "fetch_failed"
		}
		return res
	}
	_, changed, err := m.Store.ApplyAutoProfile(person, map[string]string{
		FieldRealName:     prof.Name,
		FieldOrganization: NormalizeCompany(prof.Company),
		FieldLocation:     prof.Location,
	})
	if err != nil {
		res.Skipped = "store_error"
		return res
	}
	if changed {
		res.Filled = 1
	}
	return res
}
