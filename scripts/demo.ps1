# NEXUS demonstrations.
#
#   .\scripts\demo.ps1 pipeline      Trigger a run and follow it to completion
#   .\scripts\demo.ps1 failure       Deploy a build that fails health verification
#   .\scripts\demo.ps1 rollback      Roll production back to its last healthy release
#   .\scripts\demo.ps1 kill-worker   Kill a worker mid-job and watch the lease recover it
#   .\scripts\demo.ps1 load          Submit many runs at once and watch the queue drain
#
# Each of these exercises a real code path. Nothing here is simulated for the demo.

param([Parameter(Position = 0)][string]$Scenario = 'help')

$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $PSScriptRoot
$Cli = Join-Path $Root 'bin\nexus.exe'
$Api = $env:NEXUS_API_URL; if (-not $Api) { $Api = 'http://localhost:8080' }

function Get-Token {
  if ($env:NEXUS_TOKEN) { return $env:NEXUS_TOKEN }
  $body = @{ email = 'admin@nexus.local'; password = 'nexus-demo-password' } | ConvertTo-Json
  $res = Invoke-RestMethod -Uri "$Api/api/v1/auth/login" -Method Post -Body $body -ContentType 'application/json'
  return $res.token
}

function Require-Cli {
  if (-not (Test-Path $Cli)) { throw "Build the CLI first: .\scripts\dev.ps1 build" }
}

switch ($Scenario) {
  'pipeline' {
    Require-Cli
    $env:NEXUS_TOKEN = Get-Token
    Write-Host "Projects:"
    & $Cli project list
    $projectId = (& $Cli project list | Select-Object -Skip 1 | Select-Object -First 1).Split(' ')[0]
    Write-Host "`nTriggering a run for $projectId`n"
    $out = & $Cli pipeline run $projectId
    Write-Host $out
    $runId = ($out | Select-String 'run_\w+').Matches.Value
    if ($runId) { & $Cli pipeline watch $runId }
  }

  'failure' {
    $env:NEXUS_TOKEN = Get-Token
    Write-Host @"
Deploying a build whose health endpoint returns 503.

The apply will succeed. Verification will not: the prober needs consecutive
passes and never gets one, so the deployment is recorded FAILED with every
failed probe retained as evidence, and rollback becomes available.
"@
    Require-Cli
    & $Cli environment list
    Write-Host "`nPick a production environment id above, then:"
    Write-Host "  docker run -d --name nexus-bad -p 31999:8080 -e FAIL_HEALTHCHECK=true <image>"
    Write-Host "  .\bin\nexus.exe deployment rollback <environment-id>"
  }

  'rollback' {
    Require-Cli
    $env:NEXUS_TOKEN = Get-Token
    Write-Host "Environments:"
    & $Cli environment list
    Write-Host "`nRolling back the first environment listed..."
    $envId = (& $Cli environment list | Select-Object -Skip 1 | Select-Object -First 1).Split(' ')[0]
    if ($envId) {
      & $Cli deployment rollback $envId
      Write-Host "`nThe rollback ran the same apply and the same verification as any deployment."
      Write-Host "Check the audit trail for who requested it: $Api/console/audit"
    }
  }

  'kill-worker' {
    $LogDir = Join-Path $Root '.runlogs'
    $pidFile = Join-Path $LogDir 'worker-1.pid'
    if (-not (Test-Path $pidFile)) { throw "No worker is running. Start the stack with .\scripts\dev.ps1 up" }
    $workerPid = Get-Content $pidFile
    Write-Host @"
Killing worker-1 (pid $workerPid).

Any job it holds keeps its lease until it expires - up to NEXUS_WORKER_LEASE_SECONDS,
30s by default. The reconciler then returns the job to QUEUED with its attempt
count intact, and worker-2 claims it. Nothing is lost and nothing is duplicated.
"@
    Stop-Process -Id $workerPid -Force
    Remove-Item $pidFile -ErrorAction SilentlyContinue
    Write-Host "`nKilled. Watch the reconciler notice:"
    Write-Host "  Get-Content $LogDir\reconciler.log -Wait -Tail 20"
  }

  'load' {
    Require-Cli
    $env:NEXUS_TOKEN = Get-Token
    $projectId = (& $Cli project list | Select-Object -Skip 1 | Select-Object -First 1).Split(' ')[0]
    Write-Host "Submitting 20 runs to $projectId..."
    1..20 | ForEach-Object { & $Cli pipeline run $projectId | Out-Null; Write-Host "  queued $_" }
    Write-Host "`nQueue depth and worker utilisation:"
    & $Cli status
    Write-Host "`nWatch it drain at $Api/console/infrastructure"
  }

  default {
    Get-Content $PSCommandPath | Select-Object -First 10 | ForEach-Object { $_ -replace '^#\s?', '' }
  }
}
