[CmdletBinding()]
param(
    [string]$Version = "latest",
    [string]$Repository = "DavidDiaz0317/hive",
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "Hive"),
    [string]$ReleaseDir = "",
    [switch]$SkipAttestation,
    [switch]$NoPath,
    [switch]$NoCodexSkill,
    [switch]$FunctionsOnly
)

$script:HiveTrustedDistributionValidatorNode = ""

function Install-Hive {
$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$installTimer = [Diagnostics.Stopwatch]::StartNew()

$InstallDir = Assert-SafeHiveInstallPath -Path $InstallDir

if (-not [Environment]::Is64BitOperatingSystem) {
    throw "The integrated Hive installer currently supports Windows x64 only."
}
if (-not (Get-Command tar.exe -ErrorAction SilentlyContinue)) {
    throw "The Windows tar.exe archive tool is required for bounded integrated release extraction."
}
if (-not $ReleaseDir -and -not (Get-Command gh -ErrorAction SilentlyContinue)) {
    throw "GitHub CLI is required for signed artifact verification and the one-time GitHub authorization flow. Install it from https://cli.github.com/."
}
if ($Repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') {
    throw "Repository must be an owner/name pair containing only letters, numbers, dots, underscores, or hyphens."
}
if (-not $ReleaseDir) {
    Invoke-HiveNativeCommand -FilePath "gh" -ArgumentList @("auth", "status") -Operation "GitHub CLI authentication check" | Out-Null
}
if ($Version -eq "latest") {
    if ($ReleaseDir) { throw "A concrete --Version is required with --ReleaseDir." }
    $versionOutput = @(Invoke-HiveNativeCommand -FilePath "gh" -ArgumentList @("api", "repos/$Repository/releases/latest", "--jq", ".tag_name") -Operation "GitHub latest release lookup")
    $Version = [string]::Join("`n", $versionOutput).Trim()
    if (-not $Version) { throw "No published integrated Hive release was found." }
}
if ($Version -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$') {
    throw "Release version contains unsafe characters."
}
if (-not $ReleaseDir -and $Version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$') {
    throw "Published integrated release versions must match vMAJOR.MINOR.PATCH-integrated.NUMBER."
}
$releaseCommit = ""
if (-not $ReleaseDir) {
    $releaseCommit = Resolve-HiveReleaseCommit -Repository $Repository -Version $Version
}

$asset = "hive-integrated-$Version-windows-amd64.zip"
$checksumAsset = "$asset.sha256"
$tempRoot = Join-Path ([IO.Path]::GetTempPath()) ("hive-install-" + [guid]::NewGuid().ToString("N"))
$downloadDir = Join-Path $tempRoot "download"
$extractDir = Join-Path $tempRoot "extract"
$stagingDir = "$InstallDir.new-$([guid]::NewGuid().ToString('N'))"
$backupDir = "$InstallDir.previous"
$hadPreviousInstall = Test-Path -LiteralPath $InstallDir
$activated = $false
$committed = $false
$originalUserPath = [Environment]::GetEnvironmentVariable("Path", "User")
$originalProcessPath = $env:Path
$pathChanged = $false
$skillTarget = ""
$skillBackup = ""
$skillHadPrevious = $false
$skillBackupCompleted = $false
$skillCopyStarted = $false
$transitionJournal = "$InstallDir.installer-transition.json"
$transitionHelper = Join-Path $tempRoot "hive-transition.exe"
$transitionPrepared = $false
$transitionCompleted = $false

try {
    New-Item -ItemType Directory -Path $downloadDir, $extractDir -Force | Out-Null
    if ($ReleaseDir) {
        Copy-Item -LiteralPath (Join-Path $ReleaseDir $asset) -Destination $downloadDir
        Copy-Item -LiteralPath (Join-Path $ReleaseDir $checksumAsset) -Destination $downloadDir
    } else {
        Invoke-HiveNativeCommand -FilePath "gh" -ArgumentList @("release", "download", $Version, "--repo", $Repository, "--pattern", $asset, "--pattern", $checksumAsset, "--dir", $downloadDir) -Operation "GitHub release download" | Out-Null
    }
    Write-Verbose ("Hive archive acquired at {0:n2}s" -f $installTimer.Elapsed.TotalSeconds)
    $archive = Join-Path $downloadDir $asset
    $checksum = Join-Path $downloadDir $checksumAsset
    if (-not $SkipAttestation) {
        if (-not (Get-Command gh -ErrorAction SilentlyContinue)) { throw "GitHub CLI is required for signed artifact verification." }
        Invoke-HiveNativeCommand -FilePath "gh" -ArgumentList @(
            "attestation", "verify", $archive,
            "--repo", $Repository,
            "--signer-workflow", "$Repository/.github/workflows/integrated-release.yml",
            "--source-ref", "refs/tags/$Version",
            "--source-digest", $releaseCommit,
            "--signer-digest", $releaseCommit,
            "--deny-self-hosted-runners"
        ) -Operation "GitHub artifact attestation verification" | Out-Null
        $currentReleaseCommit = Resolve-HiveReleaseCommit -Repository $Repository -Version $Version
        if ($currentReleaseCommit -ne $releaseCommit) {
            throw "Release tag $Version moved from $releaseCommit to $currentReleaseCommit during verification."
        }
    } elseif (-not $ReleaseDir) {
        throw "--SkipAttestation is allowed only with an explicit local --ReleaseDir."
    }

    $expected = ((Get-Content -LiteralPath $checksum -Raw).Trim() -split '\s+')[0].ToLowerInvariant()
    $actual = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($expected -ne $actual) { throw "Release checksum verification failed." }
    Write-Verbose ("Hive archive provenance/checksum verified at {0:n2}s" -f $installTimer.Elapsed.TotalSeconds)
    Expand-HiveArchiveSafely -Archive $archive -Destination $extractDir
    Write-Verbose ("Hive archive extracted at {0:n2}s" -f $installTimer.Elapsed.TotalSeconds)
    $source = Get-ChildItem -LiteralPath $extractDir -Directory | Select-Object -First 1
    if (-not $source) { throw "The integrated Hive archive has no distribution directory." }
    Test-HiveDistribution -Root $source.FullName -ExpectedOS "windows" -ExpectedArchitecture "amd64" -ExpectedVersion $Version
    Write-Verbose ("Hive source inventory verified at {0:n2}s" -f $installTimer.Elapsed.TotalSeconds)
	if ($hadPreviousInstall) {
		Assert-HiveRecognizedDistribution -Path $InstallDir -Label "existing install target" -ExpectedOS "windows" -ExpectedArchitecture "amd64"
	}
	if (Test-Path -LiteralPath $backupDir) {
		Assert-HiveRecognizedDistribution -Path $backupDir -Label "existing .previous backup" -ExpectedOS "windows" -ExpectedArchitecture "amd64"
	}

    if (-not $NoCodexSkill) {
        $codexHome = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $HOME ".codex" }
        $skillsDir = Join-Path $codexHome "skills"
        $skillTarget = Join-Path $skillsDir "hive"
        $skillBackup = "$skillTarget.previous-$([guid]::NewGuid().ToString('N'))"
        if (Test-Path -LiteralPath $skillTarget) {
            $recognizedSkills = @((Join-Path $source.FullName "skills\hive"))
            if (Test-Path -LiteralPath $InstallDir) {
                Assert-HiveRecognizedDistribution -Path $InstallDir -Label "existing install target" -ExpectedOS "windows" -ExpectedArchitecture "amd64"
                $recognizedSkills += (Join-Path $InstallDir "skills\hive")
            }
            Assert-HiveRecognizedSkill -Path $skillTarget -RecognizedSources $recognizedSkills
        }
        if (Test-Path -LiteralPath $skillBackup) {
            throw "Refusing to overwrite pre-existing Codex skill backup $skillBackup; move it aside manually after inspection."
        }
    }

    if (Test-Path -LiteralPath $stagingDir) { throw "Refusing to overwrite pre-existing staging path $stagingDir; remove it manually after inspection." }
    # The signed archive was extracted and completely validated already. Move
    # its single root into the install parent instead of copying tens of
    # thousands of bundled runtime files through PowerShell one at a time.
    # The final staging->active rename remains the atomic activation boundary.
    Move-Item -LiteralPath $source.FullName -Destination $stagingDir
    $script:HiveTrustedDistributionValidatorNode = Join-Path $stagingDir "runtime\node.exe"
    Test-HiveDistribution -Root $stagingDir -ExpectedOS "windows" -ExpectedArchitecture "amd64" -ExpectedVersion $Version
    Invoke-HiveBundledNodeCommand -FilePath (Join-Path $stagingDir "runtime\node.exe") -ArgumentList @((Join-Path $stagingDir "visual-hive\visual-hive.mjs"), "--version") -Operation "Bundled Visual Hive runtime check" | Out-Null
	$candidateExecutable = Join-Path $stagingDir "hive.exe"
	Copy-Item -LiteralPath $candidateExecutable -Destination $transitionHelper
	if ($hadPreviousInstall) {
		$transitionPrepared = $true
		# Use the already-validated staged executable on the successful path. The
		# separate helper remains available only if activation must roll back.
		Invoke-HiveExactExecutableCommand -FilePath $candidateExecutable -ArgumentList @(
			"installer-transition", "--phase", "prepare", "--journal", $transitionJournal,
			"--owned-install-dir", $InstallDir, "--json"
		) -Operation "quiesce exact persistent Hive schedulers before release activation" | Out-Null
	} elseif (Test-Path -LiteralPath $transitionJournal) {
		throw "A prior Hive scheduler transition journal remains at $transitionJournal but no exact previous installation is available. Restore the prior recognized installation or recover with its original signed installer."
	}
    Write-Verbose ("Hive staging/runtime verified at {0:n2}s" -f $installTimer.Elapsed.TotalSeconds)

    Activate-HiveDistribution -StagingDir $stagingDir -InstallDir $InstallDir -BackupDir $backupDir -ExpectedOS "windows" -ExpectedArchitecture "amd64"
    $activated = $true
    $script:HiveTrustedDistributionValidatorNode = Join-Path $InstallDir "runtime\node.exe"
    Write-Verbose ("Hive distribution activated at {0:n2}s" -f $installTimer.Elapsed.TotalSeconds)

    if (-not $NoPath) {
        $userPath = $originalUserPath
        $entries = @($userPath -split ';' | Where-Object { $_ -and -not [string]::Equals($_.TrimEnd('\'), $InstallDir.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase) })
        $updatedUserPath = (@($InstallDir) + $entries) -join ';'
        if (-not [string]::Equals([string]$userPath, $updatedUserPath, [StringComparison]::OrdinalIgnoreCase)) {
            [Environment]::SetEnvironmentVariable("Path", $updatedUserPath, "User")
            $pathChanged = $true
        }
        $processEntries = @($env:Path -split ';' | Where-Object { $_ -and -not [string]::Equals($_.TrimEnd('\'), $InstallDir.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase) })
        $env:Path = (@($InstallDir) + $processEntries) -join ';'
    }
    if (-not $NoCodexSkill) {
        New-Item -ItemType Directory -Path $skillsDir -Force | Out-Null
        if (Test-Path -LiteralPath $skillTarget) {
            Move-Item -LiteralPath $skillTarget -Destination $skillBackup
            $skillHadPrevious = $true
            $skillBackupCompleted = $true
        }
        $skillCopyStarted = $true
        Copy-Item -LiteralPath (Join-Path $InstallDir "skills\hive") -Destination $skillTarget -Recurse
    }
	if ($transitionPrepared) {
		Invoke-HiveExactExecutableCommand -FilePath (Join-Path $InstallDir "hive.exe") -ArgumentList @(
			"installer-transition", "--phase", "commit", "--journal", $transitionJournal,
			"--runtime-executable", (Join-Path $InstallDir "hive.exe"), "--json"
		) -Operation "restart exact persistent Hive schedulers on the activated release" | Out-Null
	}
	# Commit the install immediately after the scheduler restart succeeds. The
	# durable commit-complete journal remains rollback-capable before this line
	# and recovery-capable after it, closing interruption windows around cleanup.
	$committed = $true
	$transitionCompleted = $transitionPrepared
	if ($transitionPrepared) {
		try {
			Invoke-HiveExactExecutableCommand -FilePath (Join-Path $InstallDir "hive.exe") -ArgumentList @(
				"installer-transition", "--phase", "finalize", "--journal", $transitionJournal, "--json"
			) -Operation "finalize exact persistent Hive scheduler release transition" | Out-Null
		} catch {
			Write-Warning "Hive and its persistent schedulers are running on the activated release, but the durable transition journal remains for automatic recovery: $($_.Exception.Message)"
		}
	}
    Write-Verbose ("Hive PATH/skill transaction committed at {0:n2}s" -f $installTimer.Elapsed.TotalSeconds)
    # Backups are retained through the complete install/PATH/skill transaction.
    # Cleanup after the commit point is best effort; a leftover backup is safer
    # than rolling back a working install because cleanup itself was denied.
    if ($skillBackup -and (Test-Path -LiteralPath $skillBackup)) {
        try { Remove-SafeTree -Path $skillBackup -Parent ([IO.Path]::GetDirectoryName($skillTarget)) } catch { Write-Warning "Hive installed, but the prior Codex skill backup remains at $skillBackup" }
    }
    if (Test-Path -LiteralPath $backupDir) {
        try {
            Assert-HiveRecognizedDistribution -Path $backupDir -Label "committed Hive backup" -ExpectedOS "windows" -ExpectedArchitecture "amd64"
            Remove-SafeTree -Path $backupDir -Parent ([IO.Path]::GetDirectoryName($InstallDir))
        } catch { Write-Warning "Hive installed, but the prior installation backup remains at $backupDir because it could not be proved to be an exact Hive distribution: $($_.Exception.Message)" }
    }
    Write-Output "Hive $Version installed at $InstallDir"
    $exactHiveCommand = Join-Path $InstallDir "hive.exe"
    Write-Output "Next (after 'gh auth login' only if needed): & `"$exactHiveCommand`" setup --repo owner/repository --coverage comprehensive --automation auto-merge --provider codex --visual-hive --start --json"
} catch {
    $installError = $_.Exception.Message
    $rollbackErrors = @()
    if ($activated -and -not $committed) {
        try {
            if ($pathChanged) { [Environment]::SetEnvironmentVariable("Path", $originalUserPath, "User") }
            $env:Path = $originalProcessPath
        } catch { $rollbackErrors += "PATH restore: $($_.Exception.Message)" }
        try {
            # Do not touch a pre-existing skill when its backup Move-Item failed.
            # Once the backup completed (or a copy began with no prior skill),
            # removing the candidate is safe and required for rollback.
            if (($skillBackupCompleted -or $skillCopyStarted) -and $skillTarget -and (Test-Path -LiteralPath $skillTarget)) {
                Remove-SafeTree -Path $skillTarget -Parent ([IO.Path]::GetDirectoryName($skillTarget))
            }
            if ($skillBackupCompleted -and $skillHadPrevious -and (Test-Path -LiteralPath $skillBackup)) {
                Move-Item -LiteralPath $skillBackup -Destination $skillTarget
            }
        } catch { $rollbackErrors += "Codex skill restore: $($_.Exception.Message)" }
        try {
            $candidateRemoved = $true
            if (Test-Path -LiteralPath $InstallDir) {
                try {
                    Assert-HiveRecognizedDistribution -Path $InstallDir -Label "newly activated Hive candidate" -ExpectedOS "windows" -ExpectedArchitecture "amd64"
                    Remove-SafeTree -Path $InstallDir -Parent ([IO.Path]::GetDirectoryName($InstallDir))
                } catch {
                    $candidateRemoved = $false
                    throw
                }
            }
            if ($candidateRemoved -and $hadPreviousInstall -and (Test-Path -LiteralPath $backupDir)) {
                Assert-HiveRecognizedDistribution -Path $backupDir -Label "Hive rollback backup" -ExpectedOS "windows" -ExpectedArchitecture "amd64"
                Move-Item -LiteralPath $backupDir -Destination $InstallDir
            }
        } catch { $rollbackErrors += "installation restore: $($_.Exception.Message)" }
    }
	if ($transitionPrepared -and -not $transitionCompleted -and (Test-Path -LiteralPath $transitionJournal)) {
		try {
			$rollbackRuntime = Join-Path $InstallDir "hive.exe"
			Invoke-HiveExactExecutableCommand -FilePath $transitionHelper -ArgumentList @(
				"installer-transition", "--phase", "rollback", "--journal", $transitionJournal,
				"--runtime-executable", $rollbackRuntime, "--json"
			) -Operation "restore exact persistent Hive schedulers after installer rollback" | Out-Null
		} catch { $rollbackErrors += "scheduler restore: $($_.Exception.Message)" }
	}
	if ($rollbackErrors.Count -gt 0) {
		throw "Hive installation failed ($installError) and rollback was incomplete: $($rollbackErrors -join '; ')"
	}
	if ($activated -and -not $committed) {
		throw "Hive installation failed ($installError); the previous installation, PATH, and Codex skill were restored; persistent schedulers were also restored."
	}
    throw
} finally {
    if (Test-Path -LiteralPath $tempRoot) { Remove-SafeTree -Path $tempRoot -Parent ([IO.Path]::GetTempPath()) }
    if (Test-Path -LiteralPath $stagingDir) { Remove-SafeTree -Path $stagingDir -Parent ([IO.Path]::GetDirectoryName($InstallDir)) }
}
}

function Invoke-HiveNativeCommand {
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [string[]]$ArgumentList = @(),
        [Parameter(Mandatory)][string]$Operation
    )
    $output = & $FilePath @ArgumentList
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        throw "$Operation failed with exit code $exitCode."
    }
    return $output
}

function ConvertTo-HiveWindowsProcessArgument {
    param([AllowEmptyString()][string]$Value)
    if ($Value.Length -gt 32767) { throw "Hive native command argument is excessive." }
    if ($Value.Length -gt 0 -and $Value -notmatch '[\s"]') { return $Value }
    $builder = New-Object System.Text.StringBuilder
    [void]$builder.Append('"')
    $backslashes = 0
    foreach ($character in $Value.ToCharArray()) {
        if ($character -eq [char]92) {
            $backslashes++
            continue
        }
        if ($character -eq '"') {
            if ($backslashes -gt 0) { [void]$builder.Append([char]92, $backslashes * 2) }
            [void]$builder.Append([char]92)
            [void]$builder.Append('"')
            $backslashes = 0
            continue
        }
        if ($backslashes -gt 0) {
            [void]$builder.Append([char]92, $backslashes)
            $backslashes = 0
        }
        [void]$builder.Append($character)
    }
    if ($backslashes -gt 0) { [void]$builder.Append([char]92, $backslashes * 2) }
    [void]$builder.Append('"')
    return $builder.ToString()
}

function Invoke-HiveExactExecutableCommand {
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [string[]]$ArgumentList = @(),
        [Parameter(Mandatory)][string]$Operation,
        [int]$TimeoutSeconds = 120
    )
    $resolvedExecutable = [IO.Path]::GetFullPath($FilePath)
    $executableItem = Get-Item -LiteralPath $resolvedExecutable -Force
    if ($executableItem.PSIsContainer -or ($executableItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -or $executableItem.Extension -ine '.exe') {
        throw "$Operation requires an exact regular Windows executable."
    }
    if ($TimeoutSeconds -lt 1 -or $TimeoutSeconds -gt 600) { throw "$Operation timeout is outside the supported range." }
    $startInfo = New-Object System.Diagnostics.ProcessStartInfo
    $startInfo.FileName = $resolvedExecutable
    $startInfo.Arguments = (@($ArgumentList | ForEach-Object { ConvertTo-HiveWindowsProcessArgument -Value ([string]$_) }) -join ' ')
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $startInfo
    $started = $false
    try {
        $started = $process.Start()
        if (-not $started) { throw "$Operation did not start." }
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit($TimeoutSeconds * 1000)) {
            $process.Kill()
            $process.WaitForExit()
            throw "$Operation exceeded its $TimeoutSeconds-second timeout."
        }
        $process.WaitForExit()
        $stdout = $stdoutTask.Result.Trim()
        $stderr = $stderrTask.Result.Trim()
        $exitCode = $process.ExitCode
    } catch {
        if ($started -and -not $process.HasExited) {
            $process.Kill()
            $process.WaitForExit()
        }
        throw
    } finally {
        $process.Dispose()
    }
    if ($exitCode -ne 0) {
        $detail = if ($stderr) { $stderr } elseif ($stdout) { $stdout } else { "no command output" }
        throw "$Operation failed with exit code $exitCode`: $detail"
    }
    if (-not $stdout) { return @() }
    return @($stdout -split '\r?\n')
}

function Invoke-HiveBundledNodeCommand {
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [string[]]$ArgumentList = @(),
        [Parameter(Mandatory)][string]$Operation
    )
    $savedNodeOptions = [Environment]::GetEnvironmentVariable("NODE_OPTIONS", "Process")
    $savedNodePath = [Environment]::GetEnvironmentVariable("NODE_PATH", "Process")
    try {
        [Environment]::SetEnvironmentVariable("NODE_OPTIONS", $null, "Process")
        [Environment]::SetEnvironmentVariable("NODE_PATH", $null, "Process")
        return Invoke-HiveNativeCommand -FilePath $FilePath -ArgumentList $ArgumentList -Operation $Operation
    } finally {
        [Environment]::SetEnvironmentVariable("NODE_OPTIONS", $savedNodeOptions, "Process")
        [Environment]::SetEnvironmentVariable("NODE_PATH", $savedNodePath, "Process")
    }
}

function Expand-HiveArchiveSafely {
    param(
        [Parameter(Mandatory)][string]$Archive,
        [Parameter(Mandatory)][string]$Destination
    )
    $archivePath = [IO.Path]::GetFullPath($Archive)
    $destinationPath = [IO.Path]::GetFullPath($Destination).TrimEnd('\', '/')
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zipReader = [IO.Compression.ZipFile]::OpenRead($archivePath)
    $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $archiveRoot = ""
    [int64]$expandedBytes = 0
    try {
        if ($zipReader.Entries.Count -eq 0 -or $zipReader.Entries.Count -gt 50000) {
            throw "The integrated release archive has an empty or excessive entry inventory."
        }
        foreach ($zipEntry in $zipReader.Entries) {
            $entry = ([string]$zipEntry.FullName).Replace('\', '/').TrimEnd('/')
            if (-not $entry) { continue }
            if ($entry.Contains(':') -or $entry.StartsWith('/') -or [IO.Path]::IsPathRooted($entry)) {
                throw "Unsafe integrated release archive path: $entry"
            }
            $parts = @($entry -split '/')
            if ($parts.Count -eq 0 -or @($parts | Where-Object { -not $_ -or $_ -eq '.' -or $_ -eq '..' }).Count -gt 0) {
                throw "Unsafe integrated release archive path: $entry"
            }
            if (-not $archiveRoot) { $archiveRoot = $parts[0] }
            if ($parts[0] -cne $archiveRoot) { throw "The integrated release archive must contain exactly one root directory." }
            if (-not $seen.Add($entry)) { throw "Duplicate integrated release archive path: $entry" }
            $expandedBytes += [int64]$zipEntry.Length
            if ($expandedBytes -gt 1GB) { throw "The integrated release archive exceeds the 1 GiB expanded-byte limit." }
            $unixType = ([int64]$zipEntry.ExternalAttributes -shr 16) -band 0xF000
            if ($unixType -ne 0 -and $unixType -ne 0x4000 -and $unixType -ne 0x8000) {
                throw "The integrated release archive contains a link or unsupported entry type: $entry"
            }
        }
    } finally {
        $zipReader.Dispose()
    }
    if (-not $archiveRoot) { throw "The integrated release archive has no root directory." }
    $verboseEntries = @(Invoke-HiveNativeCommand -FilePath "tar.exe" -ArgumentList @("-tvf", $archivePath) -Operation "integrated release archive type inventory")
    if ($verboseEntries.Count -ne $seen.Count) { throw "The integrated release archive inventories disagree." }
    foreach ($verboseEntry in $verboseEntries) {
        $type = ([string]$verboseEntry).Substring(0, 1)
        if ($type -ne "-" -and $type -ne "d") { throw "The integrated release archive contains a link or unsupported tar entry type." }
    }
    Invoke-HiveNativeCommand -FilePath "tar.exe" -ArgumentList @("-xf", $archivePath, "-C", $destinationPath) -Operation "integrated release archive extraction" | Out-Null
}

function Resolve-HiveReleaseCommit {
    param(
        [Parameter(Mandatory)][string]$Repository,
        [Parameter(Mandatory)][string]$Version
    )
    $output = @(Invoke-HiveNativeCommand -FilePath "gh" -ArgumentList @("api", "repos/$Repository/commits/$Version", "--jq", ".sha") -Operation "GitHub release tag resolution")
    $commit = [string]::Join("`n", $output).Trim().ToLowerInvariant()
    if ($commit -notmatch '^[a-f0-9]{40}$') {
        throw "Release tag $Version did not resolve to an exact 40-character commit."
    }
    return $commit
}

function Activate-HiveDistribution {
    param(
        [Parameter(Mandatory)][string]$StagingDir,
        [Parameter(Mandatory)][string]$InstallDir,
        [Parameter(Mandatory)][string]$BackupDir,
        [Parameter(Mandatory)][string]$ExpectedOS,
        [Parameter(Mandatory)][string]$ExpectedArchitecture
    )
    $InstallDir = Assert-SafeHiveInstallPath -Path $InstallDir
    $parent = [IO.Path]::GetDirectoryName($InstallDir)
    $hadPrevious = Test-Path -LiteralPath $InstallDir
    Assert-HiveRecognizedDistribution -Path $StagingDir -Label "staged Hive candidate" -ExpectedOS $ExpectedOS -ExpectedArchitecture $ExpectedArchitecture
    if ($hadPrevious) {
        Assert-HiveRecognizedDistribution -Path $InstallDir -Label "existing install target" -ExpectedOS $ExpectedOS -ExpectedArchitecture $ExpectedArchitecture
    }
    if (Test-Path -LiteralPath $BackupDir) {
        Assert-HiveRecognizedDistribution -Path $BackupDir -Label "existing .previous backup" -ExpectedOS $ExpectedOS -ExpectedArchitecture $ExpectedArchitecture
    }
    if ($hadPrevious -and (Test-Path -LiteralPath $BackupDir)) {
        Remove-SafeTree -Path $BackupDir -Parent $parent
    }
    if ($hadPrevious) {
        Move-Item -LiteralPath $InstallDir -Destination $BackupDir
    }
    try {
        Move-Item -LiteralPath $StagingDir -Destination $InstallDir
    } catch {
        $activationError = $_.Exception.Message
        if ($hadPrevious -and (Test-Path -LiteralPath $BackupDir)) {
            try {
                if (Test-Path -LiteralPath $InstallDir) {
                    Assert-HiveRecognizedDistribution -Path $InstallDir -Label "partially activated Hive candidate" -ExpectedOS $ExpectedOS -ExpectedArchitecture $ExpectedArchitecture
                    Remove-SafeTree -Path $InstallDir -Parent $parent
                }
                Assert-HiveRecognizedDistribution -Path $BackupDir -Label "Hive rollback backup" -ExpectedOS $ExpectedOS -ExpectedArchitecture $ExpectedArchitecture
                Move-Item -LiteralPath $BackupDir -Destination $InstallDir
            } catch {
                $restoreError = $_.Exception.Message
                throw "Hive activation failed ($activationError) and the previous installation could not be restored ($restoreError). The backup remains at $BackupDir."
            }
            throw "Hive activation failed ($activationError); the previous installation was restored."
        }
        throw "Hive activation failed: $activationError"
    }
}

function Test-HiveDistribution {
    param([Parameter(Mandatory)][string]$Root, [string]$ExpectedOS, [string]$ExpectedArchitecture, [string]$ExpectedVersion = "")
    $resolvedRoot = [IO.Path]::GetFullPath($Root)
    $rootItem = Get-Item -LiteralPath $resolvedRoot -Force
    if (-not $rootItem.PSIsContainer -or ($rootItem.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw "The integrated Hive distribution root must be a real directory, not a file, symlink, or junction."
    }
    $manifestPath = Join-Path $resolvedRoot "distribution-manifest.json"
    $manifestItem = Get-Item -LiteralPath $manifestPath -Force
    if ($manifestItem.PSIsContainer -or ($manifestItem.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "The integrated Hive distribution manifest is not a regular file." }
    if ($manifestItem.Length -le 0 -or $manifestItem.Length -gt 64MB) { throw "The integrated Hive distribution manifest is empty or excessive." }
    # Bootstrap only the bundled Node executable with PowerShell before using
    # it as the fast validator. Published archives have already passed exact
    # checksum and attestation verification; this one-file digest prevents a
    # corrupt executable from running before the complete inventory check.
    $validatorNode = $script:HiveTrustedDistributionValidatorNode
    if (-not $validatorNode -or -not (Test-Path -LiteralPath $validatorNode -PathType Leaf)) {
        # Windows PowerShell 5.1's ConvertFrom-Json is extremely slow for the
        # complete file inventory. Read only the stable, generated Node entry
        # needed for bootstrap; the verified Node process then parses and
        # validates every manifest field and file below. The published archive
        # itself has already passed exact checksum and attestation verification.
        $manifestText = [IO.File]::ReadAllText($manifestPath)
        $nodePattern = '(?s)\{\s*"path"\s*:\s*"runtime/node\.exe"\s*,\s*"sha256"\s*:\s*"([a-f0-9]{64})"\s*,\s*"size"\s*:\s*([1-9][0-9]*)\s*\}'
        $nodeEntries = [regex]::Matches($manifestText, $nodePattern)
        if ($nodeEntries.Count -ne 1) {
            throw "The integrated distribution lacks one exact bootstrappable runtime/node.exe inventory entry."
        }
        $expectedNodeDigest = $nodeEntries[0].Groups[1].Value
        [int64]$expectedNodeSize = 0
        if (-not [int64]::TryParse($nodeEntries[0].Groups[2].Value, [ref]$expectedNodeSize) -or $expectedNodeSize -le 0) { throw "The bundled Node validator inventory size is invalid." }
        $validatorNode = Join-Path $resolvedRoot "runtime\node.exe"
        $nodeItem = Get-Item -LiteralPath $validatorNode -Force
        if ($nodeItem.PSIsContainer -or ($nodeItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -or $nodeItem.Length -ne $expectedNodeSize) {
            throw "The bundled Node validator is not the exact regular inventoried file."
        }
        $nodeDigest = (Get-FileHash -LiteralPath $validatorNode -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($nodeDigest -ne $expectedNodeDigest) { throw "The bundled Node validator digest is invalid." }
        $nodeVersionOutput = @(Invoke-HiveBundledNodeCommand -FilePath $validatorNode -ArgumentList @("--version") -Operation "Bundled Node runtime check")
        $nodeVersion = [string]::Join("`n", $nodeVersionOutput).Trim()
        if ($nodeVersion -notmatch '^v22\.[0-9]+\.[0-9]+$') {
            throw "Bundled Node runtime check must report an exact Node 22 version; got '$nodeVersion'."
        }
    }
    Invoke-HiveDistributionNodeValidator -ValidatorNode $validatorNode -Root $resolvedRoot -ExpectedOS $ExpectedOS -ExpectedArchitecture $ExpectedArchitecture -ExpectedVersion $ExpectedVersion
    $script:HiveTrustedDistributionValidatorNode = $validatorNode
}

function Invoke-HiveDistributionNodeValidator {
    param(
        [Parameter(Mandatory)][string]$ValidatorNode,
        [Parameter(Mandatory)][string]$Root,
        [Parameter(Mandatory)][string]$ExpectedOS,
        [Parameter(Mandatory)][string]$ExpectedArchitecture,
        [string]$ExpectedVersion = ""
    )
    $validatorProgram = @'
const crypto = require("node:crypto");
const fs = require("node:fs");
const path = require("node:path");
(async () => {
const suppliedRoot = path.resolve(process.env.HIVE_VALIDATOR_ROOT || "");
const expectedOS = process.env.HIVE_VALIDATOR_OS;
const expectedArchitecture = process.env.HIVE_VALIDATOR_ARCHITECTURE;
const expectedVersion = process.env.HIVE_VALIDATOR_VERSION || "";
const rootStat = fs.lstatSync(suppliedRoot);
if (!rootStat.isDirectory() || rootStat.isSymbolicLink()) throw new Error("distribution root is not an ordinary directory");
const root = fs.realpathSync(suppliedRoot);
const manifestPath = path.join(root, "distribution-manifest.json");
const manifestStat = fs.lstatSync(manifestPath);
if (!manifestStat.isFile() || manifestStat.isSymbolicLink() || manifestStat.size > 64 * 1024 * 1024) throw new Error("distribution manifest is not a bounded regular file");
const manifest = JSON.parse(fs.readFileSync(manifestPath, "utf8"));
if (manifest.schema_version !== "hive.integrated-distribution.v1" || manifest.os !== expectedOS || manifest.architecture !== expectedArchitecture) throw new Error("distribution platform mismatch");
if (!/^[a-f0-9]{40}$/u.test(manifest.hive_commit || "") || !/^[a-f0-9]{40}$/u.test(manifest.visual_hive_commit || "") || manifest.hosted_controller_protocol !== 1 || !/^v22\./u.test(manifest.node_version || "") || !Array.isArray(manifest.files) || manifest.files.length < 1 || manifest.files.length > 50000) throw new Error("distribution identity is incomplete or excessive");
if (expectedVersion && manifest.hive_version !== expectedVersion) throw new Error(`distribution Hive version ${manifest.hive_version || "<missing>"} does not match requested release ${expectedVersion}`);
if (manifest.hive_version && !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/u.test(manifest.hive_version)) throw new Error("distribution Hive version is unsafe");
const inventoried = new Map();
const inventoryEntries = [];
for (const file of manifest.files) {
  const relative = String(file.path || "");
  const parts = relative.split("/");
  if (!relative || path.posix.isAbsolute(relative) || relative.includes("\\") || relative.includes(":") || parts.some(part => !part || part === "." || part === "..")) throw new Error(`unsafe inventory path: ${relative}`);
  if (!Number.isSafeInteger(file.size) || file.size < 0 || !/^[a-f0-9]{64}$/u.test(file.sha256 || "")) throw new Error(`invalid inventory identity: ${relative}`);
  const identity = relative.toLowerCase();
  if (inventoried.has(identity)) throw new Error(`Duplicate distribution inventory path: ${relative}`);
  inventoried.set(identity, relative);
  inventoryEntries.push({file, parts, relative});
}
const digestFile = target => new Promise((resolve, reject) => {
  const hash = crypto.createHash("sha256");
  const stream = fs.createReadStream(target);
  stream.on("error", reject);
  stream.on("data", chunk => hash.update(chunk));
  stream.on("end", () => resolve(hash.digest("hex")));
});
let inventoryCursor = 0;
const validateInventoryEntry = async () => {
  while (inventoryCursor < inventoryEntries.length) {
    const index = inventoryCursor++;
    const {file, parts, relative} = inventoryEntries[index];
    const target = path.join(root, ...parts);
    const stat = await fs.promises.lstat(target);
    if (!stat.isFile() || stat.isSymbolicLink() || stat.size !== file.size) throw new Error(`inventory size mismatch or non-regular file: ${relative}`);
    const realTarget = await fs.promises.realpath(target);
    if (!realTarget.toLowerCase().startsWith(root.toLowerCase() + path.sep)) throw new Error(`inventory path escaped root: ${relative}`);
    const digest = await digestFile(target);
    if (digest !== file.sha256) throw new Error(`inventory digest mismatch: ${relative}`);
  }
};
await Promise.all(Array.from({length: Math.min(16, inventoryEntries.length)}, validateInventoryEntry));
const actual = new Map();
let entries = 0;
const walk = directory => {
  for (const entry of fs.readdirSync(directory, {withFileTypes: true})) {
    if (++entries > 100000) throw new Error("distribution entry-count limit exceeded");
    const target = path.join(directory, entry.name);
    const stat = fs.lstatSync(target);
    const relative = path.relative(root, target).split(path.sep).join("/");
    if (stat.isSymbolicLink()) throw new Error(`distribution contains symlink or junction: ${relative}`);
    if (stat.isDirectory()) walk(target);
    else if (stat.isFile()) {
      const identity = relative.toLowerCase();
      if (actual.has(identity)) throw new Error(`duplicate Windows filesystem path: ${relative}`);
      actual.set(identity, relative);
    } else throw new Error(`distribution contains unsupported entry: ${relative}`);
  }
};
walk(root);
const expected = new Map(inventoried);
expected.set("distribution-manifest.json", "distribution-manifest.json");
for (const [identity, relative] of actual) if (!expected.has(identity)) throw new Error(`Uninventoried distribution file: ${relative}`);
for (const [identity, relative] of expected) if (!actual.has(identity)) throw new Error(`missing distribution file: ${relative}`);
const requiredFiles = ["hive.exe", "runtime/node.exe", "visual-hive/visual-hive.mjs", "visual-hive/release-manifest.json", "skills/hive/SKILL.md", "skills/hive/agents/openai.yaml"];
if (expectedVersion || inventoried.has("visual-hive.cmd")) requiredFiles.push("visual-hive.cmd");
for (const required of requiredFiles) {
  const stat = fs.lstatSync(path.join(root, ...required.split("/")));
  if (!stat.isFile() || stat.isSymbolicLink()) throw new Error(`missing regular file ${required}`);
}
process.stdout.write("HIVE_DISTRIBUTION_VALIDATED\n");
})().catch(error => {
  process.stderr.write(`${error && error.stack ? error.stack : String(error)}\n`);
  process.exitCode = 1;
});
'@
    $startInfo = New-Object System.Diagnostics.ProcessStartInfo
    $startInfo.FileName = $ValidatorNode
    $startInfo.Arguments = "-"
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardInput = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    $startInfo.EnvironmentVariables.Remove("NODE_OPTIONS")
    $startInfo.EnvironmentVariables.Remove("NODE_PATH")
    $startInfo.EnvironmentVariables["HIVE_VALIDATOR_ROOT"] = [IO.Path]::GetFullPath($Root)
    $startInfo.EnvironmentVariables["HIVE_VALIDATOR_OS"] = $ExpectedOS
    $startInfo.EnvironmentVariables["HIVE_VALIDATOR_ARCHITECTURE"] = $ExpectedArchitecture
    $startInfo.EnvironmentVariables["HIVE_VALIDATOR_VERSION"] = $ExpectedVersion
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $startInfo
    $started = $false
    try {
        $started = $process.Start()
        if (-not $started) { throw "Bundled distribution validator did not start." }
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        $process.StandardInput.Write($validatorProgram)
        $process.StandardInput.Close()
        if (-not $process.WaitForExit(120000)) {
            $process.Kill()
            $process.WaitForExit()
            throw "Bundled distribution validator exceeded its 120-second timeout."
        }
        $process.WaitForExit()
        $output = $stdoutTask.Result.Trim()
        $errorOutput = $stderrTask.Result.Trim()
        $exitCode = $process.ExitCode
    } catch {
        if ($started -and -not $process.HasExited) {
            $process.Kill()
            $process.WaitForExit()
        }
        throw
    } finally {
        $process.Dispose()
    }
    if ($exitCode -ne 0 -or $output -ne "HIVE_DISTRIBUTION_VALIDATED") {
        $detail = if ($errorOutput) { $errorOutput } elseif ($output) { $output } else { "validator exited without output" }
        throw "Integrated distribution inventory validation failed: $detail"
    }
}

function Assert-HiveRecognizedDistribution {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label,
        [Parameter(Mandatory)][string]$ExpectedOS,
        [Parameter(Mandatory)][string]$ExpectedArchitecture
    )
    try {
        Test-HiveDistribution -Root $Path -ExpectedOS $ExpectedOS -ExpectedArchitecture $ExpectedArchitecture
    } catch {
        throw "Refusing to replace $Label at $Path because it is not an exact recognized Hive distribution. Move it aside manually or choose a new dedicated -InstallDir; no existing bytes were changed. Details: $($_.Exception.Message)"
    }
}

function Get-HiveSkillInventory {
    param([Parameter(Mandatory)][string]$Path)
    $root = [IO.Path]::GetFullPath($Path).TrimEnd('\', '/')
    $rootItem = Get-Item -LiteralPath $root -Force
    if (-not $rootItem.PSIsContainer -or ($rootItem.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw "Hive skill root must be a real directory."
    }
    $prefix = $root + [IO.Path]::DirectorySeparatorChar
    $rows = [Collections.Generic.List[string]]::new()
    [int64]$bytes = 0
    foreach ($entry in @(Get-ChildItem -LiteralPath $root -Recurse -Force | Sort-Object FullName)) {
        $full = [IO.Path]::GetFullPath($entry.FullName)
        if (-not $full.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) { throw "Hive skill path escaped its root." }
        $relative = $full.Substring($prefix.Length).Replace('\', '/')
        if ($rows.Count -ge 512) { throw "Hive skill entry-count limit exceeded." }
        if ($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "Hive skill contains a symlink or junction: $relative" }
        if ($entry.PSIsContainer) {
            $rows.Add("D`0$relative")
        } else {
            if ($rows.Count -ge 256) { throw "Hive skill file-count limit exceeded." }
            $bytes += [int64]$entry.Length
            if ($bytes -gt 4MB) { throw "Hive skill byte limit exceeded." }
            $digest = (Get-FileHash -LiteralPath $entry.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
            $rows.Add("$relative`0$($entry.Length)`0$digest")
        }
    }
    return [string]::Join("`n", $rows)
}

function Assert-HiveRecognizedSkill {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string[]]$RecognizedSources
    )
    try {
        $actual = Get-HiveSkillInventory -Path $Path
        foreach ($source in $RecognizedSources) {
            if ($actual -ceq (Get-HiveSkillInventory -Path $source)) { return }
        }
    } catch {
        throw "Refusing to replace Codex skill at $Path because it is not an exact packaged Hive skill. Move it aside manually or use -NoCodexSkill; no existing bytes were changed. Details: $($_.Exception.Message)"
    }
    throw "Refusing to replace Codex skill at $Path because it is not an exact packaged Hive skill. Move it aside manually or use -NoCodexSkill; no existing bytes were changed."
}

function Assert-SafeHiveInstallPath {
    param([Parameter(Mandatory)][string]$Path)
    if (-not [IO.Path]::IsPathRooted($Path) -or $Path -match '^[A-Za-z]:[^\\/]') {
        throw "Hive -InstallDir must be an absolute path to a dedicated Hive directory."
    }
    $resolved = [IO.Path]::GetFullPath($Path).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
    $root = [IO.Path]::GetPathRoot($resolved).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
    if (-not $resolved -or [string]::Equals($resolved, $root, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to install Hive at a filesystem root. Choose a new dedicated Hive directory."
    }
    $commonParents = @(
        $HOME, $env:USERPROFILE, $env:LOCALAPPDATA, $env:APPDATA, $env:ProgramData,
        $env:ProgramFiles, ${env:ProgramFiles(x86)}, $env:SystemRoot, [IO.Path]::GetTempPath(),
        (Join-Path $HOME ".codex"), (Join-Path $HOME ".config"), (Join-Path $HOME ".cache")
    ) | Where-Object { $_ } | ForEach-Object { [IO.Path]::GetFullPath([string]$_).TrimEnd('\', '/') }
    foreach ($common in $commonParents) {
        if ([string]::Equals($resolved, $common, [StringComparison]::OrdinalIgnoreCase)) {
            throw "Refusing unsafe Hive install directory $resolved because it is a home or common parent directory. Choose a new dedicated Hive leaf."
        }
    }
    $leaf = [IO.Path]::GetFileName($resolved)
    if ($leaf -notmatch '(?i)^(?:\.?hive(?:[-_.].*)?|.*[-_.]hive(?:[-_.].*)?|installed|install[-_.].*)$') {
        throw "Refusing non-dedicated Hive install leaf '$leaf'. Use a directory named Hive, Hive-*, *-Hive, installed, or install-*."
    }
    return $resolved
}

function Remove-SafeTree {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Parent)
    $resolvedPath = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $resolvedParent = [IO.Path]::GetFullPath($Parent).TrimEnd('\')
    if (-not $resolvedPath.StartsWith($resolvedParent + '\', [StringComparison]::OrdinalIgnoreCase) -or $resolvedPath -eq $resolvedParent) {
        throw "Refusing to remove path outside the intended parent: $resolvedPath"
    }
    for ($attempt = 0; $attempt -lt 8; $attempt++) {
        if (-not (Test-Path -LiteralPath $resolvedPath)) { return }
        try {
            Remove-Item -LiteralPath $resolvedPath -Recurse -Force -ErrorAction Stop
            return
        } catch {
            if ($attempt -eq 7) { throw }
            $delayMilliseconds = [Math]::Min(1000, 100 * [Math]::Pow(2, $attempt))
            Start-Sleep -Milliseconds $delayMilliseconds
        }
    }
}

if (-not $FunctionsOnly) {
    Install-Hive
}
