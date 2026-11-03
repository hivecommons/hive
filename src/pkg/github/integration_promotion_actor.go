package github

import "fmt"

// VerifyIntegrationPromotionActor rejects PAT/ambient-user substitution. The
// actual protected qualification uses this same installed App identity.
func (c *Client) VerifyIntegrationPromotionActor(appID, installationID int64) error {
	if c == nil || c.appAuth == nil || appID <= 0 || installationID <= 0 ||
		c.appAuth.appID != appID || c.appAuth.InstallationID() != installationID {
		return fmt.Errorf("integration promotion actor is not the explicitly qualified GitHub App installation")
	}
	return nil
}
