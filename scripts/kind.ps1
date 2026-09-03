# The local Kubernetes cluster NEXUS deploys into.
#
#   .\scripts\kind.ps1 create       Create the nexus-local cluster
#   .\scripts\kind.ps1 bootstrap    Create it and prepare the namespaces NEXUS uses
#   .\scripts\kind.ps1 status       Cluster, nodes, and every NEXUS-managed workload
#   .\scripts\kind.ps1 workloads    What NEXUS currently has running, with revisions
#   .\scripts\kind.ps1 load-image   Load a locally built image into the cluster
#   .\scripts\kind.ps1 logs         Logs from a deployed workload
#   .\scripts\kind.ps1 delete       Delete the cluster
#
# Nothing here needs a cloud account, a registry, or a paid service.

param(
  [Parameter(Position = 0)][string]$Command = 'help',
  [Parameter(Position = 1)][string]$Argument = ''
)

# Continue, not Stop. kind and kubectl write progress to stderr and still exit 0, and
# under 'Stop' PowerShell promotes any native stderr output to a terminating error - so
# an ordinary, successful cluster creation would abort the script. Exit codes are
# checked explicitly instead, which is what actually signals failure for a native tool.
$ErrorActionPreference = 'Continue'

$Root = Split-Path -Parent $PSScriptRoot
$ClusterName = 'nexus-local'
$KubeContext = "kind-$ClusterName"
$ConfigFile = Join-Path $Root 'deployments\kubernetes\kind-cluster.yaml'

# The namespaces NEXUS deploys the seeded projects into. Created up front so the first
# deployment is not also the first namespace creation, which keeps a failure there
# unambiguous.
$Namespaces = @(
  'checkout-api-production', 'checkout-api-staging', 'checkout-api-development',
  'ledger-service-production', 'ledger-service-staging', 'ledger-service-development',
  'storefront-web-production', 'storefront-web-staging', 'storefront-web-development'
)

function Require-Tool($name, $hint) {
  if (-not (Get-Command $name -ErrorAction SilentlyContinue)) {
    Write-Error "$name is not on PATH. $hint"
    exit 1
  }
}

function Check-Exit($what) {
  if ($LASTEXITCODE -ne 0) {
    Write-Error "$what failed (exit $LASTEXITCODE)"
    exit 1
  }
}

function Cluster-Exists {
  # "No kind clusters found." arrives on stderr, so it is filtered out rather than
  # mistaken for a cluster name.
  $clusters = & kind get clusters 2>&1 |
    Where-Object { $_ -notmatch 'No kind clusters' } |
    ForEach-Object { "$_".Trim() }
  return $clusters -contains $ClusterName
}

function Require-Cluster {
  if (-not (Cluster-Exists)) {
    Write-Error "The $ClusterName cluster does not exist. Create it with: .\scripts\kind.ps1 bootstrap"
    exit 1
  }
}

function Kubectl { & kubectl --context $KubeContext @args }

switch ($Command) {

  'create' {
    Require-Tool kind 'Install with: winget install Kubernetes.kind'
    Require-Tool kubectl 'Install with: winget install Kubernetes.kubectl'
    Require-Tool docker 'Install Docker Desktop.'

    if (Cluster-Exists) {
      Write-Host "Cluster $ClusterName already exists."
    } else {
      Write-Host "Creating cluster $ClusterName (this takes about a minute)..."
      & kind create cluster --name $ClusterName --config $ConfigFile 2>&1 |
        ForEach-Object { Write-Host "  $_" }
      Check-Exit 'kind create cluster'
    }

    Write-Host ""
    Write-Host "Waiting for the node to be ready..."
    Kubectl wait --for=condition=Ready nodes --all --timeout=180s 2>&1 |
      ForEach-Object { Write-Host "  $_" }
    Check-Exit 'waiting for the node'
    Kubectl get nodes 2>&1 | ForEach-Object { Write-Host $_ }
  }

  'bootstrap' {
    & $PSCommandPath create
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host ""
    Write-Host "Creating namespaces..."
    foreach ($ns in $Namespaces) {
      $existing = Kubectl get namespace $ns --ignore-not-found -o name 2>&1 |
        Where-Object { $_ -match 'namespace/' }
      if (-not $existing) {
        Kubectl create namespace $ns 2>&1 | Out-Null
        Write-Host "  created $ns"
      } else {
        Write-Host "  $ns already present"
      }
    }

    Write-Host ""
    Write-Host "Cluster $ClusterName is ready."
    Write-Host ""
    Write-Host "  Context      $KubeContext"
    Write-Host "  Node ports   30080 production | 30081 staging | 30082 development | 30083 preview"
    Write-Host ""
    Write-Host "Point NEXUS at it by starting the control plane with:"
    Write-Host ""
    Write-Host "  `$env:NEXUS_DEPLOY_PROVIDER = 'kubernetes'"
    Write-Host "  `$env:NEXUS_KUBE_CONTEXT    = '$KubeContext'"
    Write-Host "  `$env:NEXUS_KIND_CLUSTER    = '$ClusterName'"
  }

  'status' {
    Require-Tool kubectl 'Install kubectl.'
    if (-not (Cluster-Exists)) {
      Write-Host "Cluster $ClusterName does not exist."
      Write-Host "Create it with: .\scripts\kind.ps1 bootstrap"
      return
    }

    Write-Host "=== cluster ==="
    Kubectl cluster-info 2>&1 | ForEach-Object { Write-Host $_ }
    Write-Host ""
    Write-Host "=== nodes ==="
    Kubectl get nodes -o wide 2>&1 | ForEach-Object { Write-Host $_ }
    Write-Host ""
    Write-Host "=== NEXUS-managed workloads ==="
    Kubectl get deployments --all-namespaces -l app.kubernetes.io/managed-by=nexus 2>&1 |
      ForEach-Object { Write-Host $_ }
    Write-Host ""
    Write-Host "=== pods ==="
    Kubectl get pods --all-namespaces -l app.kubernetes.io/managed-by=nexus -o wide 2>&1 |
      ForEach-Object { Write-Host $_ }
    Write-Host ""
    Write-Host "=== services ==="
    Kubectl get services --all-namespaces -l app.kubernetes.io/managed-by=nexus 2>&1 |
      ForEach-Object { Write-Host $_ }
  }

  'workloads' {
    Require-Cluster
    $columns = 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,' +
      'REVISION:.spec.template.metadata.labels.nexus\.dev/revision,' +
      'IMAGE:.spec.template.spec.containers[0].image,' +
      'READY:.status.readyReplicas,DESIRED:.spec.replicas'
    Kubectl get deployments --all-namespaces -l app.kubernetes.io/managed-by=nexus -o $columns 2>&1 |
      ForEach-Object { Write-Host $_ }
  }

  'load-image' {
    Require-Cluster
    if (-not $Argument) {
      Write-Error "Usage: .\scripts\kind.ps1 load-image <image:tag>"
      exit 1
    }
    Write-Host "Loading $Argument into $ClusterName..."
    & kind load docker-image $Argument --name $ClusterName 2>&1 |
      ForEach-Object { Write-Host "  $_" }
    Check-Exit "kind load docker-image $Argument"
    Write-Host "Loaded."
  }

  'logs' {
    Require-Cluster
    if (-not $Argument) {
      Write-Host "Usage: .\scripts\kind.ps1 logs <namespace>"
      Write-Host ""
      Write-Host "Namespaces with NEXUS workloads:"
      Kubectl get deployments --all-namespaces -l app.kubernetes.io/managed-by=nexus `
        -o 'custom-columns=NAMESPACE:.metadata.namespace' --no-headers 2>&1 |
        Sort-Object -Unique | ForEach-Object { Write-Host "  $_" }
      return
    }
    Kubectl logs -n $Argument -l app.kubernetes.io/managed-by=nexus --tail=100 --all-containers 2>&1 |
      ForEach-Object { Write-Host $_ }
  }

  'delete' {
    if (-not (Cluster-Exists)) {
      Write-Host "Cluster $ClusterName does not exist."
      return
    }
    Write-Host "Deleting cluster $ClusterName..."
    & kind delete cluster --name $ClusterName 2>&1 | ForEach-Object { Write-Host "  $_" }
    Write-Host "Deleted. Nothing else on this machine is affected."
  }

  default {
    Get-Content $PSCommandPath | Select-Object -First 11 | ForEach-Object { $_ -replace '^#\s?', '' }
  }
}
