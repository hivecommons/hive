package api

// HostSetter is one row of HostSetters.
type HostSetter struct {
	Kind HostSetterKind
	Via  string
}
