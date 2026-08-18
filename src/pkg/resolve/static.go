package resolve

func init() {
	RegisterResolver("static", newStaticResolver)
}

// staticResolver resolves a variable to a fixed literal value declared in
// config. Always available (no policy gate).
type staticResolver struct {
	value string
}

func newStaticResolver(spec VarSpec, _ Policy) (Resolver, error) {
	return &staticResolver{value: spec.Value}, nil
}

func (r *staticResolver) Resolve(_ Request) (Result, error) {
	return Result{Value: r.value, Source: "static"}, nil
}
