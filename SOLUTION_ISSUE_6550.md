# Solution for Issue #6550

## 🛠️ Proposed Solution (by Aditya Waghamare)

### Analysis
Claude Code CLI when initialized via subscription/OAuth requires specific onboarding flags or configuration persistence (`hasCompletedOnboarding`, settings JSON, or credential environment mapping) to bypass the interactive first-run login prompt when run inside containerized relays. When running in headless/relay mode, if `hasCompletedOnboarding` or configuration keys are missing or not pre-seeded, the CLI halts at the "Select login method" prompt, triggering a readiness timeout.

### Fix
Update the relay initialization sequence and configuration prep scripts in `hivecommons/hive` contributor setup to automatically seed `hasCompletedOnboarding` and ensure credential persistence paths correctly link OAuth configuration and credentials so `claude` skips the interactive prompt on container startup.

### Implementation
```typescript
// contrib/relay/src/cli/claude.ts / startup scripts
export function ensureClaudeOnboardingConfig(homeDir: string): void {
  const configDir = path.join(homeDir, '.claude');
  fs.mkdirSync(configDir, { recursive: true });
  
  const settingsPath = path.join(configDir, 'config.json');
  let settings: Record<string, any> = {};
  
  if (fs.existsSync(settingsPath)) {
    try {
      settings = JSON.parse(fs.readFileSync(settingsPath, 'utf8'));
    } catch {
      settings = {};
    }
  }

  // Ensure onboarding flag is set so non-interactive/relay sessions bypass login chooser
  if (settings.hasCompletedOnboarding === undefined) {
    settings.hasCompletedOnboarding = true;
    fs.writeFileSync(settingsPath, JSON.stringify(settings, null, 2), { mode: 0o600 });
  }
}
```

### Testing
Verify by running `just contribute-setup claude` with valid OAuth credentials mounted, ensuring `hasCompletedOnboarding` is injected into `~/.claude/config.json` before starting the tmux session, avoiding the "Select login method" blockage.

Signed-off-by: Aditya Waghamare <adityawaghamare7620@gmail.com>


---
*Submitted by Aditya Waghamare*
💰 **Payout Address (Base L2 / EVM):** `0xb61dBcdBc3407F71EaCb64D4CBFAcf9FFfe2415C`