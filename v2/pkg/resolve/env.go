package resolve

import "os"

func init() {
	RegisterResolver("env", newEnvResolver)
}

// envResolver resolves a variable from an environment variable. It is the
// always-on default that reproduces the legacy os.LookupEnv behavior: a set
// variable (even to empty string) resolves to its value; an unset variable is
// ErrUnresolved unless the spec supplies a default.
type envResolver struct {
	spec VarSpec
}

func newEnvResolver(spec VarSpec, _ Policy) (Resolver, error) {
	return &envResolver{spec: spec}, nil
}

func (r *envResolver) Resolve(req Request) (Result, error) {
	name := r.spec.Env
	if name == "" {
		name = req.Name
	}
	if val, ok := os.LookupEnv(name); ok {
		return Result{Value: val, Source: "env:" + name}, nil
	}
	if r.spec.HasDefault {
		return Result{Value: r.spec.Default, Source: "default"}, nil
	}
	return Result{}, ErrUnresolved
}

// bareEnvResolver is the implicit resolver used for any ${NAME} that has no
// operator VarSpec: look up the environment variable of the same name, leave
// literal if unset. This is exactly the legacy config.expandEnvVars behavior.
type bareEnvResolver struct{}

func (bareEnvResolver) Resolve(req Request) (Result, error) {
	if val, ok := os.LookupEnv(req.Name); ok {
		return Result{Value: val, Source: "env:" + req.Name}, nil
	}
	return Result{}, ErrUnresolved
}
