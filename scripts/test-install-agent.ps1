# test-install-agent.ps1 — behavioral tests for scripts/install-agent.ps1.
#
# Serves a fixture release over a local HttpListener stub and asserts the
# installer's integrity-verification contract on Windows:
#
#   - manifest unreachable                      -> fails closed
#   - manifest ok, artifact absent              -> fails closed
#   - manifest with wrong sha256                -> "checksum mismatch"
#   - manifest artifact missing sha256 field    -> "integrity verification unavailable"
#   - manifest with correct sha256              -> installs
#   - LOCALMESH_AGENT_BIN (local binary path)   -> installs, no downloads
#
# Wired in by ci-agent-smoke.sh on windows-latest (pwsh -File).

$ErrorActionPreference = 'Stop'
$pass = 0; $failCount = 0
function Ok($m)   { $script:pass++; Write-Host "  PASS $m" }
function Bad($m,$d){ $script:failCount++; Write-Host "  FAIL $m — $d" }

$root = Split-Path -Parent $PSScriptRoot
$installer = Join-Path $root "scripts/install-agent.ps1"
$T = Join-Path $env:TEMP ("lm-ps1test-" + [guid]::NewGuid().ToString('N'))
$fixt = Join-Path $T "fixtures"
New-Item -ItemType Directory -Force $fixt | Out-Null

$ART = "localmesh-agent-9.9.9-windows-amd64.exe"
Set-Content -Path (Join-Path $fixt $ART) -Value "fake-agent-exe" -NoNewline
Copy-Item (Join-Path $fixt $ART) (Join-Path $fixt "payload.bin")
$ART_SHA = (Get-FileHash (Join-Path $fixt $ART) -Algorithm SHA256).Hash.ToLower()
$BAD_SHA = '0' * 64

function Write-FixtureManifest([string]$mode, [string]$sha) {
  switch ($mode) {
    'correct' { $arts = @(@{file=$ART;os='windows';arch='amd64';sha256=$sha;size=14}) }
    'nosha'   { $arts = @(@{file=$ART;os='windows';arch='amd64';size=14}) }
    default   { $arts = @(@{file='other';os='plan9';arch='mips';sha256=$BAD_SHA;size=14}) }
  }
  @{version='9.9.9';protocol_version=1;artifacts=$arts} |
    ConvertTo-Json -Depth 5 | Set-Content (Join-Path $fixt 'release-manifest.json')
}

# --- fixture HTTP stub ------------------------------------------------------
# A single-file-per-path server: serves $fixt/<path> or 404s. The manifest
# additionally honors manifest_mode=down (always 404).
$tmp = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
$tmp.Start(); $port = $tmp.LocalEndpoint.Port; $tmp.Stop()
$job = Start-Job -ScriptBlock {
  param($port, $fixt)
  $l = [System.Net.HttpListener]::new()
  $l.Prefixes.Add("http://127.0.0.1:$port/")
  $l.Start()
  while ($l.IsListening) {
    $ctx = $l.GetContext()   # blocks until a request arrives
    $p = $ctx.Request.Url.LocalPath.TrimStart('/')
    if ($p -eq 'release-manifest.json' -and
        (Get-Content (Join-Path $fixt 'manifest_mode') -ErrorAction SilentlyContinue) -eq 'down') {
      $ctx.Response.StatusCode = 404; $ctx.Response.Close(); continue
    }
    $f = Join-Path $fixt $p
    if (Test-Path $f) {
      $b = [IO.File]::ReadAllBytes($f)
      $ctx.Response.OutputStream.Write($b, 0, $b.Length)
    } else {
      $ctx.Response.StatusCode = 404
    }
    $ctx.Response.Close()
  }
} -ArgumentList $port, $fixt

$base = "http://127.0.0.1:$port"

# run_install <args> -> $global:__out, $global:__err
function Run-Install([string[]]$arguments, [string]$binOverride) {
  $case = Join-Path $T ("app" + (Get-Random))
  New-Item -ItemType Directory -Force $case | Out-Null
  $env:LOCALAPPDATA = $case
  $env:LOCALMESH_DOWNLOAD_BASE = $base
  if ($binOverride) {
    $env:LOCALMESH_AGENT_BIN = $binOverride
  } else {
    Remove-Item Env:LOCALMESH_AGENT_BIN -ErrorAction SilentlyContinue
  }
  $global:__err = ""
  try {
    $global:__out = (& $installer -NoJoin @arguments 2>&1 | Out-String)
  } catch {
    $global:__out = "$_"
    $global:__err = "$_"
  }
  Remove-Item Env:LOCALMESH_DOWNLOAD_BASE -ErrorAction SilentlyContinue
  Remove-Item Env:LOCALMESH_AGENT_BIN -ErrorAction SilentlyContinue
  $script:lastApp = $case
}
$script:lastApp = ""

try {

# 1. manifest unreachable -> fail closed
'down' | Set-Content (Join-Path $fixt 'manifest_mode')
Run-Install @()
if ($__err -match 'could not fetch release manifest') { Ok "manifest unreachable → fails closed" }
else { Bad "manifest unreachable should fail" "err: $__err" }

# 2. manifest ok, artifact absent -> fails closed
'ok' | Set-Content (Join-Path $fixt 'manifest_mode')
Write-FixtureManifest 'absent' ''
Run-Install @()
if ($__err -match 'no windows/amd64 artifact') { Ok "artifact absent from manifest → fails closed" }
else { Bad "absent-artifact should fail" "err: $__err" }

# 3. wrong sha256 -> checksum mismatch
Write-FixtureManifest 'correct' $BAD_SHA
Run-Install @()
if ($__err -match 'checksum mismatch') { Ok "wrong sha256 → checksum mismatch" }
else { Bad "wrong sha256 should fail" "err: $__err" }

# 4. artifact lacks sha256 field -> integrity verification unavailable
Write-FixtureManifest 'nosha' ''
Run-Install @()
if ($__err -match 'integrity verification unavailable') { Ok "missing sha256 field → fails closed" }
else { Bad "missing sha256 field should fail closed" "err: $__err" }

# 5. correct sha256 -> installs
Write-FixtureManifest 'correct' $ART_SHA
Run-Install @()
$installed = Join-Path $script:lastApp "LocalMesh\localmesh-agent.exe"
if (-not $__err -and (Test-Path $installed)) { Ok "correct sha256 → installs" }
else { Bad "happy-path install failed" "err: $__err" }

# 6. LOCALMESH_AGENT_BIN bypasses downloads entirely
Run-Install @() (Join-Path $fixt $ART)
$installed = Join-Path $script:lastApp "LocalMesh\localmesh-agent.exe"
if (-not $__err -and (Test-Path $installed)) { Ok "LOCALMESH_AGENT_BIN local path → installs" }
else { Bad "local-binary path failed" "err: $__err" }

} finally {
  Stop-Job $job -ErrorAction SilentlyContinue | Out-Null
  Remove-Job $job -Force -ErrorAction SilentlyContinue | Out-Null
  Remove-Item $T -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host ""
Write-Host "install-agent.ps1 tests: $pass passed, $failCount failed"
exit ($failCount -eq 0 ? 0 : 1)
