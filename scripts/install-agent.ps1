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
  [string]$Repo = "localmesh/localmesh",
  [switch]$NoJoin,
  [switch]$Service
)

$ErrorActionPreference = "Stop"

$dest = Join-Path $env:LOCALAPPDATA "LocalMesh"
$bin  = Join-Path $dest "localmesh-agent.exe"
New-Item -ItemType Directory -Force -Path $dest | Out-Null

if ($env:LOCALMESH_AGENT_BIN -and (Test-Path $env:LOCALMESH_AGENT_BIN)) {
  Write-Host "  using local binary: $env:LOCALMESH_AGENT_BIN"
  Copy-Item $env:LOCALMESH_AGENT_BIN $bin -Force
} else {
  if ($Version -eq "latest") {
    $url = "https://github.com/$Repo/releases/latest/download/localmesh-agent-windows-amd64.exe"
  } else {
    $url = "https://github.com/$Repo/releases/download/$Version/localmesh-agent-windows-amd64.exe"
  }
  Write-Host "  downloading $url"
  Invoke-WebRequest -Uri $url -OutFile $bin -UseBasicParsing
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
    & $bin @joinArgs "-run=false"
    & $bin service install
    & $bin service start
  } else {
    & $bin @joinArgs
  }
} else {
  if ($Service) {
    $cfgdir = if ($env:LOCALMESH_AGENT_DIR) { $env:LOCALMESH_AGENT_DIR } else { Join-Path $env:USERPROFILE ".localmesh" }
    $cfg = Join-Path $cfgdir "agent.json"
    if (Test-Path $cfg) {
      Write-Host "  registering Windows service (requires elevation)"
      & $bin service install
      & $bin service start
    } else {
      Write-Host "  note: -Service given but no config at $cfg — join first"
    }
  }
  Write-Host ""
  Write-Host "Next: localmesh-agent join -server https://<control-plane>:8443 -token lme_..."
  Write-Host "Approve the enrollment in the dashboard when it appears."
  Write-Host "Optional: localmesh-agent service install   (Windows service, elevated)"
}
