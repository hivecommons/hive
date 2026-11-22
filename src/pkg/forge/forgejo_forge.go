package forge

type forgejoForge struct {
	*giteaForge
}

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
