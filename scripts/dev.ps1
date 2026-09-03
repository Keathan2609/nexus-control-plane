# NEXUS local development.
#
#   .\scripts\dev.ps1 up        Bring everything up and seed the demo estate
#   .\scripts\dev.ps1 down      Stop the processes, leave the data
#   .\scripts\dev.ps1 nuke      Stop everything and delete the volumes
#   .\scripts\dev.ps1 seed      Re-seed the demo estate
#   .\scripts\dev.ps1 test      Unit and integration tests
#   .\scripts\dev.ps1 build     Build every binary into .\bin
#   .\scripts\dev.ps1 logs      Tail the local process logs
#   .\scripts\dev.ps1 status    What is running
#
# Deliberately a script rather than a Makefile: make on Windows means installing a
# toolchain that exists only to run five commands.

param([Parameter(Position = 0)][string]$Command = 'help')

$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $PSScriptRoot
$Compose = Join-Path $Root 'deployments\docker\docker-compose.yml'
$LogDir = Join-Path $Root '.runlogs'

# Host ports are offset from the defaults so this stack coexists with other projects.
$env:NEXUS_DATABASE_URL = 'postgres://nexus:nexus@localhost:5436/nexus?sslmode=disable'
$env:NEXUS_NATS_URL = 'nats://localhost:4222'
$env:NEXUS_ENVIRONMENT = 'development'
$env:NEXUS_LOG_FORMAT = 'text'

function Require-Tool($name, $hint) {
  if (-not (Get-Command $name -ErrorAction SilentlyContinue)) {
    throw "$name is not on PATH. $hint"
  }
}

function Wait-Healthy($container, $seconds = 90) {
  for ($i = 0; $i -lt $seconds; $i++) {
    $state = (& docker inspect --format '{{.State.Health.Status}}' $container 2>$null)
    if ($state -eq 'healthy') { Write-Host "  $container healthy"; return }
    Start-Sleep -Seconds 1
  }
  throw "$container did not become healthy within ${seconds}s"
}

function Start-Background($name, $exe, $envVars) {
  New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
  $log = Join-Path $LogDir "$name.log"
  foreach ($k in $envVars.Keys) { Set-Item -Path "env:$k" -Value $envVars[$k] }
  $p = Start-Process -FilePath $exe -RedirectStandardOutput $log `
       -RedirectStandardError (Join-Path $LogDir "$name.err.log") `
       -WindowStyle Hidden -PassThru
  Set-Content -Path (Join-Path $LogDir "$name.pid") -Value $p.Id -Encoding utf8
  Write-Host "  $name started (pid $($p.Id)) -> $log"
}

function Stop-Background($name) {
  $pidFile = Join-Path $LogDir "$name.pid"
  if (Test-Path $pidFile) {
    $processId = Get-Content $pidFile
    try { Stop-Process -Id $processId -Force -ErrorAction Stop; Write-Host "  stopped $name" } catch {}
    Remove-Item $pidFile -ErrorAction SilentlyContinue
  }
}

function Build-All {
  Write-Host "Building binaries..."
  Push-Location $Root
  try {
    & go build -o bin\nexus-api.exe .\cmd\nexus-api
    if ($LASTEXITCODE -ne 0) { throw "build failed: nexus-api" }
    & go build -o bin\nexus-worker.exe .\cmd\nexus-worker
    if ($LASTEXITCODE -ne 0) { throw "build failed: nexus-worker" }
    & go build -o bin\nexus-reconciler.exe .\cmd\nexus-reconciler
    if ($LASTEXITCODE -ne 0) { throw "build failed: nexus-reconciler" }
    & go build -o bin\nexus-seed.exe .\cmd\nexus-seed
    if ($LASTEXITCODE -ne 0) { throw "build failed: nexus-seed" }
    & go build -o bin\nexus.exe .\cmd\nexus
    if ($LASTEXITCODE -ne 0) { throw "build failed: nexus (cli)" }
    Write-Host "  five binaries in .\bin"
  } finally { Pop-Location }
}

switch ($Command) {
  'up' {
    Require-Tool docker 'Install Docker Desktop.'
    Require-Tool go 'Install Go 1.27 or newer.'
    Require-Tool npm 'Install Node 20 or newer.'

    Write-Host "Starting backing services..."
    & docker compose -f $Compose up -d postgres nats prometheus grafana jaeger
    if ($LASTEXITCODE -ne 0) { throw 'docker compose failed' }
    Wait-Healthy 'nexus-postgres'
    Wait-Healthy 'nexus-nats'

    Build-All

    Write-Host "Seeding the demo estate..."
    & (Join-Path $Root 'bin\nexus-seed.exe')

    Write-Host "Starting the control plane..."
    Start-Background 'api' (Join-Path $Root 'bin\nexus-api.exe') @{
      NEXUS_HTTP_ADDR = ':8080'; NEXUS_METRICS_ADDR = ':9095'
    }
    Start-Background 'worker-1' (Join-Path $Root 'bin\nexus-worker.exe') @{
      NEXUS_METRICS_ADDR = ':9096'; NEXUS_WORKER_ID = 'wkr_local_1'; NEXUS_WORKER_CONCURRENCY = '4'
    }
    Start-Background 'worker-2' (Join-Path $Root 'bin\nexus-worker.exe') @{
      NEXUS_METRICS_ADDR = ':9098'; NEXUS_WORKER_ID = 'wkr_local_2'; NEXUS_WORKER_CONCURRENCY = '4'
    }
    Start-Background 'reconciler' (Join-Path $Root 'bin\nexus-reconciler.exe') @{
      NEXUS_METRICS_ADDR = ':9097'
    }

    Write-Host "Starting the console..."
    Push-Location (Join-Path $Root 'web')
    try {
      if (-not (Test-Path 'node_modules')) { & npm install --no-audit --no-fund }
      $web = Start-Process -FilePath 'npm.cmd' -ArgumentList 'run', 'dev' `
             -RedirectStandardOutput (Join-Path $LogDir 'web.log') `
             -RedirectStandardError (Join-Path $LogDir 'web.err.log') `
             -WindowStyle Hidden -PassThru
      Set-Content -Path (Join-Path $LogDir 'web.pid') -Value $web.Id -Encoding utf8
    } finally { Pop-Location }

    Write-Host ""
    Write-Host "NEXUS is up."
    Write-Host "  Console      http://localhost:3100"
    Write-Host "  API          http://localhost:8080"
    Write-Host "  Prometheus   http://localhost:9091"
    Write-Host "  Grafana      http://localhost:3300"
    Write-Host "  Jaeger       http://localhost:16687"
    Write-Host ""
    Write-Host "  Sign in as admin@nexus.local / nexus-demo-password"
  }

  'down' {
    Write-Host "Stopping local processes..."
    'api', 'worker-1', 'worker-2', 'reconciler', 'web' | ForEach-Object { Stop-Background $_ }
    & docker compose -f $Compose stop
    Write-Host "Stopped. Data volumes kept; use nuke to remove them."
  }

  'nuke' {
    'api', 'worker-1', 'worker-2', 'reconciler', 'web' | ForEach-Object { Stop-Background $_ }
    & docker compose -f $Compose down -v
    Write-Host "Everything removed, including volumes."
  }

  'seed' {
    Build-All
    & (Join-Path $Root 'bin\nexus-seed.exe') --reset
  }

  'build' { Build-All }

  'test' {
    Push-Location $Root
    try {
      Write-Host "Unit tests..."
      & go test ./...
      if ($LASTEXITCODE -ne 0) { throw 'unit tests failed' }

      Write-Host "Integration tests..."
      $env:NEXUS_TEST_DATABASE_URL = 'postgres://nexus:nexus@localhost:5436/nexus_test?sslmode=disable'
      & go test -tags=integration -timeout 300s ./test/integration/...
      if ($LASTEXITCODE -ne 0) { throw 'integration tests failed' }

      Write-Host "Frontend..."
      Push-Location (Join-Path $Root 'web')
      try {
        & npx tsc --noEmit
        if ($LASTEXITCODE -ne 0) { throw 'typecheck failed' }
      } finally { Pop-Location }

      Write-Host "All checks passed."
    } finally { Pop-Location }
  }

  'logs' {
    Get-ChildItem $LogDir -Filter '*.log' -ErrorAction SilentlyContinue | ForEach-Object {
      Write-Host "=== $($_.Name) ==="
      Get-Content $_.FullName -Tail 20
      Write-Host ""
    }
  }

  'status' {
    & docker compose -f $Compose ps
    Write-Host ""
    Get-ChildItem $LogDir -Filter '*.pid' -ErrorAction SilentlyContinue | ForEach-Object {
      $name = $_.BaseName
      $processId = Get-Content $_.FullName
      $running = Get-Process -Id $processId -ErrorAction SilentlyContinue
      Write-Host ("  {0,-12} {1}" -f $name, $(if ($running) { "running (pid $processId)" } else { 'stopped' }))
    }
  }

  default {
    Get-Content $PSCommandPath | Select-Object -First 14 | ForEach-Object { $_ -replace '^#\s?', '' }
  }
}
