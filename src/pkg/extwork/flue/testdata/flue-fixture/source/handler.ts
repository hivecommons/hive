export async function triage(issue: { title: string; body: string }): Promise<string> {
  if (!issue.title.trim()) {
    return "needs-title";
  }
  return issue.body.includes("reproduction") ? "ready" : "needs-repro";
}
