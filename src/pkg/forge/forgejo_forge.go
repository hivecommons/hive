package forge

// forgejoForge is the Forgejo adapter. It embeds the Gitea REST client and
// differs only in the kind it reports.
type forgejoForge struct {
	*giteaForge
}

// newForgejoForge constructs the Forgejo adapter with the same token and BaseURL
// rules as newGiteaForge.
func newForgejoForge(token string, opts Options) (*forgejoForge, error) {
	g, err := newGiteaFamilyForge(KindForgejo, token, opts)
	if err != nil {
		return nil, err
	}
	return &forgejoForge{giteaForge: g}, nil
}

func (f *forgejoForge) Kind() Kind { return KindForgejo }

var _ Forge = (*forgejoForge)(nil)
var _ Merger = (*forgejoForge)(nil)
