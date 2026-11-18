package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

// placeholderHiveNamePrefix is the display name the hub gives an unclaimed pool
// slot ("Available slot <id>"). It names inventory, not a project, so it is
// never advertised to a contributor.
const placeholderHiveNamePrefix = "Available slot"

// contributeHiveName is the hive's display name for the auth_ok frame: the
// same project.name the fleet self-report carries (hub provisioning seeds it
// from the hive's project_name), normalized so it is one safe printable line.
// "" when the hive has no name or still carries a pool-slot placeholder name,
// which makes the field vanish from the frame and leaves a relay on its
// hostname-derived short name.
func (s *Server) contributeHiveName() string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return ""
	}
	name := fleetreport.NormalizeHiveName(s.deps.Config.Project.Name)
	if strings.HasPrefix(name, placeholderHiveNamePrefix) {
		return ""
	}
	return name
}
