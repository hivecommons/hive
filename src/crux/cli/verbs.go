package cli

// RuntimeVerbs maps each crux subcommand that drives an agent runtime to the
// crux.Runtime methods it calls, in call order. It is the CLI's half of the
// parity contract in hivecommons/hive#14138: TestVerbsHaveRuntimeMethods
// asserts every method named here is on crux.Runtime and every verb dispatch
// routes to a runtime is listed here, so a verb cannot grow a dependency the
// seam does not expose (and, in stage 3, that the API does not route).
// `serve` is absent on purpose: it exposes the runtime rather than driving
// it, and its routes are covered by crux/serve.
var RuntimeVerbs = map[string][]string{
	"run":  {"AddAgent", "Start", "GetBufferOutput", "Stop"},
	"ps":   {"AllStatuses"},
	"logs": {"ResolveAgent", "GetOutput", "GetBufferOutput", "CaptureFullLog", "FullLogHeader"},
	"kick": {"ResolveAgent", "SendKick"},
	"stop": {"ResolveAgent", "Stop"},
}
