# LocalMesh agent installer for Windows (PowerShell).
#
#   powershell -ExecutionPolicy Bypass -File install-agent.ps1 `
#       -Server https://mesh.local:8443 -Token lme_...
#
# Downloads the windows-amd64 release binary and installs it to
# $env:LOCALAPPDATA\LocalMesh, then optionally joins the cluster.
# With -Service, also registers the Windows service (requires an
# elevated shell) instead of running the agent in the foreground.

[CmdletBinding()]
param(
  [string]$Server,
  [string]$Token,
  [string]$Name,
  [string]$Version = "latest",
  [string]$Repo = "Ydde21/LocalMesh",
  [switch]$NoJoin,
  [switch]$Service,
  [switch]$Help
)

if ($Help) {
  Write-Host @"
LocalMesh agent installer for Windows (PowerShell).

  powershell -ExecutionPolicy Bypass -File install-agent.ps1 ``
      -Server https://mesh.local:8443 -Token lme_...

Parameters:
  -Server <url>   control plane URL (joins after install when -Token also given)
  -Token <lme_>   enrollment token
  -Name <name>    node display name
  -Version <tag>  release tag (default: latest)
  -Repo <owner/repo>  GitHub repo to download from
  -NoJoin         install only, do not join
  -Service        register and start the Windows service (elevated)
  -Help           show this text
"@
  exit 0
}

$ErrorActionPreference = "Stop"

# SHA-256 via Get-FileHash, falling back to certutil.exe — if neither
# exists the download cannot be verified and we must not install it.
function Get-Sha256([string]$Path) {
  if (Get-Command Get-FileHash -ErrorAction SilentlyContinue) {
    return (Get-FileHash $Path -Algorithm SHA256).Hash.ToLower()
  }
  if (Get-Command certutil.exe -ErrorAction SilentlyContinue) {
    $hashLine = certutil.exe -hashfile $Path SHA256 |
      Where-Object { ($_ -replace ' ', '') -match '^[0-9a-fA-F]{64}$' } | Select-Object -First 1
    if ($hashLine) { return ($hashLine -replace ' ', '').ToLower() }
  }
  throw "no SHA-256 tool available (need Get-FileHash or certutil.exe) — cannot verify the download"
}

$dest = Join-Path $env:LOCALAPPDATA "LocalMesh"
$bin  = Join-Path $dest "localmesh-agent.exe"
New-Item -ItemType Directory -Force -Path $dest | Out-Null

if ($env:LOCALMESH_AGENT_BIN -and (Test-Path $env:LOCALMESH_AGENT_BIN)) {
  Write-Host "  using local binary: $env:LOCALMESH_AGENT_BIN"
  Copy-Item $env:LOCALMESH_AGENT_BIN $bin -Force
} else {
  # Artifacts are named localmesh-agent-<ver>-windows-amd64.exe; the
  # manifest resolves "latest" and provides the published checksum.
  # LOCALMESH_DOWNLOAD_BASE overrides the release base URL — mirrors and
  # the CI test harness use it; production installs leave it unset.
  if ($env:LOCALMESH_DOWNLOAD_BASE) {
    $base = $env:LOCALMESH_DOWNLOAD_BASE
  } elseif ($Version -eq "latest") {
    $base = "https://github.com/$Repo/releases/latest/download"
  } else {
    $base = "https://github.com/$Repo/releases/download/$Version"
  }
  $mfPath = Join-Path $env:TEMP "lm-manifest.json"
  try {
    Invoke-WebRequest -Uri "$base/release-manifest.json" -OutFile $mfPath -UseBasicParsing
  } catch {
    throw "could not fetch release manifest from $base — is the release published? (check -Repo/-Version and connectivity): $_"
  }
  $mf = Get-Content $mfPath -Raw | ConvertFrom-Json
  $artifact = ($mf.artifacts | Where-Object { $_.os -eq "windows" -and $_.arch -eq "amd64" } | Select-Object -First 1).file
  if (-not $artifact) { throw "no windows/amd64 artifact in the release manifest for $Repo@$Version" }
  Write-Host "  downloading $base/$artifact"
  try {
    Invoke-WebRequest -Uri "$base/$artifact" -OutFile $bin -UseBasicParsing
  } catch {
    throw "download failed — check that $artifact exists in release ${Version}: $_"
  }

  # Integrity verification (always — fail closed). The artifact must
  # match the release manifest checksum AND/OR a signature-verified
  # SHA256SUMS entry; at least one positive verification is required —
  # never trust transport alone (docs/releases.md).
  $verified = $false
  $want = ($mf.artifacts | Where-Object { $_.file -eq $artifact }).sha256
  if ($want) {
    $got = Get-Sha256 $bin
    if ($got -ne $want) { throw "checksum mismatch for $artifact ($got != $want) — aborting" }
    Write-Host "  checksum verified against release manifest"
    $verified = $true
  }

  # Signature verification when a release pubkey + minisign are available —
  # same contract as install-agent.sh (docs/releases.md).
  if ($env:LOCALMESH_RELEASE_PUBKEY -and (Get-Command minisign -ErrorAction SilentlyContinue)) {
    $sumsPath = Join-Path $env:TEMP "lm-sha256sums.txt"
    $sigPath  = Join-Path $env:TEMP "lm-sha256sums.minisig"
    try {
      Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile $sumsPath -UseBasicParsing
      Invoke-WebRequest -Uri "$base/SHA256SUMS.minisig" -OutFile $sigPath -UseBasicParsing
    } catch {
      throw "SHA256SUMS/signature unavailable — cannot verify signature: $_"
    }
    $pub = (Get-Content $env:LOCALMESH_RELEASE_PUBKEY -Raw).Trim()
    & minisign -V -x $sigPath -m $sumsPath -P $pub 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "release signature verification failed — refusing to install" }
    $binSum = Get-Sha256 $bin
    if (Select-String -Path $sumsPath -Pattern "$binSum" -SimpleMatch -Quiet) {
      $verified = $true
      Write-Host "  release signature verified"
    } else {
      Write-Host "  note: artifact not in signed SHA256SUMS (manifest checksum already verified)"
    }
  } else {
    Write-Host "  note: LOCALMESH_RELEASE_PUBKEY unset or minisign missing — signature verification unavailable"
  }
  if (-not $verified) {
    throw "integrity verification unavailable — could not match $artifact against the release manifest or a signed SHA256SUMS (set LOCALMESH_RELEASE_PUBKEY + install minisign to verify via signature)"
  }
}

# Add to user PATH if missing
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$dest*") {
  [Environment]::SetEnvironmentVariable("Path", "$userPath;$dest", "User")
  $env:Path = "$env:Path;$dest"
  Write-Host "  added $dest to user PATH"
}

Write-Host "  installed: $bin"

if (-not $NoJoin -and $Server -and $Token) {
  $joinArgs = @("join", "-server", $Server, "-token", $Token)
  if ($Name) { $joinArgs += @("-name", $Name) }
  if ($Service) {
    # Enroll only (no foreground run), then hand lifecycle to the SCM.
    # Native commands don't honor $ErrorActionPreference — check exits.
    & $bin @joinArgs "-run=false"
    if ($LASTEXITCODE -ne 0) { throw "join failed (exit $LASTEXITCODE) — nothing was enrolled; see the error above, then re-run join" }
    & $bin service install
    if ($LASTEXITCODE -ne 0) { throw "service install failed (exit $LASTEXITCODE) — the node is enrolled; retry: $bin service install" }
    & $bin service start
    if ($LASTEXITCODE -ne 0) { throw "service start failed (exit $LASTEXITCODE) — retry: $bin service start" }
  } else {
    & $bin @joinArgs
    exit $LASTEXITCODE
  }
} else {
  if ($Service) {
    $cfgdir = if ($env:LOCALMESH_AGENT_DIR) { $env:LOCALMESH_AGENT_DIR } else { Join-Path $env:USERPROFILE ".localmesh" }
    $cfg = Join-Path $cfgdir "agent.json"
    if (Test-Path $cfg) {
      Write-Host "  registering Windows service (requires elevation)"
      & $bin service install
      if ($LASTEXITCODE -ne 0) { throw "service install failed (exit $LASTEXITCODE) — retry: $bin service install" }
      & $bin service start
      if ($LASTEXITCODE -ne 0) { throw "service start failed (exit $LASTEXITCODE) — retry: $bin service start" }
    } else {
      Write-Host "  note: -Service given but no config at $cfg — join first"
    }
  }
  Write-Host ""
  Write-Host "Next: localmesh-agent join -server https://<control-plane>:8443 -token lme_..."
  Write-Host "Recommended: pin the cluster CA with -ca-fingerprint sha256:<hex>"
  Write-Host "  (curl -sk https://<control-plane>:8443/api/v1/ca.pem -> DER -> sha256)"
  Write-Host "Approve the enrollment in the dashboard when it appears."
  Write-Host "Optional: localmesh-agent service install   (Windows service, elevated)"
}
