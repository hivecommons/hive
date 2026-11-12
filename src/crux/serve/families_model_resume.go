package serve

import "github.com/hivecommons/hive/crux/api"

// restartFor picks the restart variant a POST /agents/{name}/restart body asks
// for. A bootstrap prompt wins; ModelChange routes through
// RestartAfterModelChange so a model-rejected pause is lifted (#17828).
func (s *Server) restartFor(name string, req api.RestartRequest) error {
	switch {
	case req.Bootstrap != "":
		return s.Runtime.RestartWithBootstrap(s.LaunchContext, name, req.Bootstrap)
	case req.ModelChange:
		return s.Runtime.RestartAfterModelChange(s.LaunchContext, name, req.By)
	default:
		return s.Runtime.Restart(s.LaunchContext, name)
	}
}
