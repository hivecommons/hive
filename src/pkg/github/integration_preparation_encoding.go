package github

import "github.com/hivecommons/hive/pkg/forgestate"

func EncodeIntegrationPreparation(request IntegrationPreparationRequest) (string, string, error) {
	return forgestate.CanonicalIntegrationPreparation(request)
}
