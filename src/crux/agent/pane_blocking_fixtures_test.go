package agent

// Fixtures shared with tests outside the blocking-prompt matcher, which now
// lives in agent/panesignal.

const codexUpdatePane = `  ✨ Update available! 0.146.0 -> 0.147.0
  Release notes: https://github.com/openai/codex/releases/latest
› 1. Update now (runs ` + "`npm install -g @openai/codex`" + `)
  2. Skip
  3. Skip until next version
  Press enter to continue`

const claudeWorkspaceTrustPane = ` Accessing workspace:

 /data/agents/guide

 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel`
