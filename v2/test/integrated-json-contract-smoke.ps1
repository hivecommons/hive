param(
    [Parameter(Mandatory)][string]$Hive,
    [Parameter(Mandatory)][string]$WorkRoot
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
New-Item -ItemType Directory -Path $WorkRoot -Force | Out-Null

$commands = @(
    "setup", "status", "doctor", "start", "stop", "daemon", "installer-transition",
    "pause", "resume", "run", "hosted-cycle", "approve-merge", "approve-baseline",
    "revoke-merge-approval", "retry-repair", "recover-dispatch", "transfer-setup-authorizer",
    "set-coverage", "set-automation", "set-issue-limit", "set-retry-limit", "upgrade",
    "rollback", "uninstall", "visual"
)

foreach ($command in $commands) {
    $start = [Diagnostics.ProcessStartInfo]::new()
    $start.FileName = [IO.Path]::GetFullPath($Hive)
    $start.UseShellExecute = $false
    $start.CreateNoWindow = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    $start.Arguments = "$command --json --hive-json-contract-invalid"
    $process = [Diagnostics.Process]::Start($start)
    $stdout = $process.StandardOutput.ReadToEnd()
    $stderr = $process.StandardError.ReadToEnd()
    if (-not $process.WaitForExit(30000)) {
        try { $process.Kill($true) } catch {}
        throw "$command packaged JSON probe timed out."
    }
    if ($process.ExitCode -eq 0) { throw "$command accepted a deliberately invalid packaged invocation." }
    try {
        $nonAscii = @($stdout.ToCharArray() | Where-Object { [int]$_ -gt 127 } | Select-Object -First 1).Count -ne 0
        if ([string]::IsNullOrWhiteSpace($stdout) -or $nonAscii) {
            throw "stdout is empty or contains non-ASCII bytes in the deterministic failure document"
        }
        $document = $stdout | ConvertFrom-Json
        if ($null -eq $document -or $document -isnot [psobject]) { throw "stdout did not decode to one JSON object" }
    } catch {
        throw "$command stdout is not exactly one UTF-8 JSON document: $($_.Exception.Message); stdout=$stdout; stderr=$stderr"
    }
    [IO.File]::WriteAllText((Join-Path $WorkRoot "$command.stdout.json"), $stdout, [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText((Join-Path $WorkRoot "$command.stderr.log"), $stderr, [Text.UTF8Encoding]::new($false))
}

Write-Host "Packaged Hive JSON contract smoke passed."
