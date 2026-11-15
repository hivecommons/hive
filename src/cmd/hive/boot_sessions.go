package main

import (
	cruxruntime "github.com/hivecommons/hive/crux/runtime"
)

// userSessionRuntime returns the boot's single Sessions runtime, creating it
// on first use with the GitHub App minter the shared workspace clone needs
// (adoptGitHubClient swaps the minter when App auth is re-established).
// Without App auth the clone is anonymous, which is enough for a public repo.
func (b *boot) userSessionRuntime() *cruxruntime.UserSessions {
	if b.userSessions == nil {
		b.userSessions = cruxruntime.NewUserSessions(b.logger)
		b.userSessions.SetCloneMinter(appMinter(b.appAuth, ""))
	}
	return b.userSessions
}
