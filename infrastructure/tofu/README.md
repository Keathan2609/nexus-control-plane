# NEXUS infrastructure (OpenTofu)

Two root modules, one shared set of child modules.

| Root | Targets | Needs an account? | Costs money? |
|---|---|---|---|
| `environments/local` | kind / k3d / minikube / Docker Desktop | No | No |
| `environments/production` | An existing EKS cluster + RDS + ECR | Yes, AWS | **Yes** |

`environments/local` is the default and the one to start with. It brings up the
whole platform — API, worker, reconciler, console, PostgreSQL 16 and NATS 2.10
with JetStream — on a cluster running on your own machine, with no cloud
account, no credentials, and no bill.

`environments/production` is opt-in. Nothing in the local path references it or
the `aws-managed` module.

Everything here works with either OpenTofu (`tofu`) or Terraform (`terraform`);
the commands below use `tofu`.

---

## Layout

```
infrastructure/tofu/
├── environments/
│   ├── local/          root module: local Kubernetes. The default.
│   └── production/     root module: AWS. Opt-in, costs money.
└── modules/
    ├── nexus-datastores/     in-cluster PostgreSQL 16 + NATS 2.10 (JetStream)
    ├── nexus-platform/       namespace, Secret, and the Helm release
    ├── nexus-observability/  Prometheus + Grafana (optional in both roots)
    └── aws-managed/          RDS, ECR, IRSA. Used only by production.
```

The workloads themselves come from the Helm chart at
`deployments/helm/nexus`, referenced by path. The chart is shared with the
plain `helm install` path and is never modified from here.

Division of responsibility, because it is easy to get wrong:

- **The chart** owns Deployments, Services, probes, the ConfigMap, PDBs,
  Ingress and NetworkPolicies.
- **`nexus-platform`** owns the namespace and the **Secret**, and passes values
  to the chart. It does not create a ConfigMap — the chart renders its own, and
  two controllers owning one object is an install failure at best and a silent
  fight at worst.

The chart's `values.schema.json` sets `additionalProperties: false` at every
level, so a value key that does not exist is a hard install-time error rather
than a setting that quietly does nothing. Two consequences worth knowing:

- `reconciler.replicaCount` **does not exist**. The reconciler is a single
  control loop with no leader election, and the schema rejects the key outright.
- The image tag `latest` is rejected by the chart template. Pin a real version.

---

## The local path, end to end

### 1. Prerequisites

- Docker (running)
- A local Kubernetes cluster
- `kubectl`
- OpenTofu ≥ 1.6

```bash
# On Windows
winget install --id OpenTofu.Tofu
winget install --id Kubernetes.kubectl
winget install --id Kubernetes.kind
```

### 2. Create a cluster

```bash
kind create cluster --name nexus
kubectl config get-contexts        # confirms the context is "kind-nexus"
```

Docker Desktop's built-in Kubernetes (`docker-desktop`) and minikube work too.
The `kube_context` variable is validated against the known local context names
specifically so that this stack cannot be pointed at a shared cluster by
accident — it creates a single-replica database with no backups.

Give the VM at least 4 CPUs and 8 GB if you can. The whole stack fits in less,
but pods sit `Pending` on a 2 GB machine and the reason is not obvious.

### 3. Build and load the images

Nothing is pulled from a registry. Images are built locally and side-loaded
into the cluster, which is why `image_pull_policy` is `IfNotPresent` and why no
registry credential exists anywhere in this path.

```bash
# From the repository root
docker build -t nexus/nexus-api:dev        -f cmd/nexus-api/Dockerfile .
docker build -t nexus/nexus-worker:dev     -f cmd/nexus-worker/Dockerfile .
docker build -t nexus/nexus-reconciler:dev -f cmd/nexus-reconciler/Dockerfile .
docker build -t nexus/nexus-web:dev        -f web/Dockerfile web

for img in api worker reconciler web; do
  kind load docker-image "nexus/nexus-$img:dev" --name nexus
done
```

The image names must be `<image_registry>/nexus-<component>:<image_tag>`.
With the defaults (`image_registry = "nexus"`, `image_tag = "dev"`) that is
exactly the four names above.

If the images are not ready yet, set `deploy_workloads = false` and apply
anyway — you get the namespace, PostgreSQL, NATS and the NEXUS Secret, which is
enough to run the Go services on your host against the in-cluster database.

### 4. Apply

```bash
cd infrastructure/tofu/environments/local
cp terraform.tfvars.example terraform.tfvars
# edit terraform.tfvars: set kube_context to your context name

tofu init
tofu plan
tofu apply
```

`kube_context` is the only required variable. Everything else has a default
that works.

### 5. Reach it

There is no Ingress: a bare kind or Docker Desktop cluster has no ingress
controller installed. Use port-forwards, which the outputs hand you directly:

```bash
tofu output next_steps
```

```bash
kubectl -n nexus port-forward svc/nexus-web 3000:3000   # console
kubectl -n nexus port-forward svc/nexus-api 8080:8080   # API
curl localhost:8080/healthz
curl localhost:8080/readyz
```

Credentials are generated during apply and never written to a file:

```bash
tofu output -raw database_url        # full DSN, for psql or a host-run binary
tofu output -raw auth_jwt_secret     # to mint a token against the local API
tofu output -raw webhook_secret
```

### 6. Optionally, metrics

```bash
tofu apply -var enable_observability=true
```

Off by default because it roughly doubles the memory the stack needs, and the
first thing most people want is a working API rather than a dashboard.

### 7. Tear down

```bash
tofu destroy
# or, to reclaim the disk as well:
kind delete cluster --name nexus
```

---

## The production path

This is the AWS path. **It creates billed resources.** Read the cost section
below first.

### What it expects to already exist

This root module deliberately does not create any of these. Each has a
lifecycle far longer than an application deploy, and a module that can delete a
VPC is a module nobody wants to run.

| Input | What it is |
|---|---|
| `vpc_id`, `database_subnet_ids` | A VPC and **private** subnets in ≥ 2 AZs |
| `eks_cluster_name` | An existing EKS cluster |
| `node_security_group_ids` | The node group SG, so pods can reach 5432 |
| `eks_oidc_provider_arn` / `_url` | The cluster's IAM OIDC provider, for IRSA |
| `nats_url` | An existing NATS 2.10 + JetStream cluster |

NATS is an input rather than something this stack runs: a Raft-quorum JetStream
cluster is a stateful system with its own upgrade and backup lifecycle, and the
copy bundled in the application chart is sized for evaluation.

### Order of operations

**1. Enable remote state.** `backend.tf` is present but commented out, so that
`tofu init -backend=false` and `tofu validate` work on any machine, including
one with no AWS account. The file contains the exact CLI commands to create the
bucket and lock table out of band — they cannot be managed by the configuration
whose state they hold — followed by `tofu init -migrate-state`.

Do this before the first real apply. This root module manages the database that
holds the system of record; local state means the only record of what exists
lives on one laptop, and two people applying at once silently overwrite each
other.

**2. Authenticate.** Credentials come from the ambient chain and never from a
variable or a file in this repository:

```bash
aws sso login --profile nexus-production
export AWS_PROFILE=nexus-production
```

**3. First apply, without workloads.** The ECR repositories do not exist yet,
so there is nothing to pull:

```bash
cd infrastructure/tofu/environments/production
cp terraform.tfvars.example terraform.tfvars   # edit it
tofu init
tofu apply -var deploy_workloads=false
```

**4. Push images** to the repositories that apply just created:

```bash
tofu output next_steps      # includes the exact ecr login command
REGISTRY=$(tofu output -raw ecr_registry)
docker tag nexus/nexus-api:0.1.0 "$REGISTRY/nexus-api:0.1.0"
docker push "$REGISTRY/nexus-api:0.1.0"
# ...and the other three
```

Tags are **immutable** in these repositories. Pushing a tag that already exists
fails, which is the point: the digest running in production cannot change
without a deploy.

**5. Full apply.**

```bash
tofu apply     # deploy_workloads defaults to true
```

`image_tag` is required and has no default. The running version of a production
system is a decision, not a fallback.

### Plan-time checks

The production root uses `check` blocks rather than only variable validation,
so problems surface during `tofu plan` instead of halfway through an apply:

- IRSA ARN set without the issuer URL (the trust policy would match nothing).
- No IRSA at all — the stack still runs, but pods have no AWS identity and
  cannot read the database credential from Secrets Manager.
- Replica counts implying more database connections than the instance budget.
- An Ingress enabled with no host, or with no TLS secret.
- An HPA floor below the declared replica count, which scales the deployment
  *down* minutes after an otherwise clean deploy.

### Known gap: IRSA annotations are not applied automatically

`aws-managed` creates one IAM role per service account and exports
`irsa_role_arns`, but the chart exposes `serviceAccount.annotations` as a single
map shared by all four service accounts. Wiring per-component role ARNs through
it would annotate every account with the same role, which defeats the point of
having separate ones. Until the chart grows a per-component annotations value,
apply them explicitly:

```bash
kubectl -n nexus annotate serviceaccount nexus-api \
  eks.amazonaws.com/role-arn="$(tofu output -json irsa_role_arns | jq -r '."nexus-api"')"
```

---

## What the AWS path costs, and why it is opt-in

Figures are **order of magnitude** for `eu-west-1` on-demand pricing at the time
of writing, for the module's defaults. They are here to make the shape of the
bill obvious, not to be quoted at anyone — always check the AWS pricing pages
and your own account's agreements.

### What this stack creates

| Resource | Default | Rough monthly |
|---|---|---|
| RDS `db.t4g.small`, single-AZ | `db_instance_class` | ~$25 |
| RDS Multi-AZ standby | `db_multi_az = true` | **~$25 more** (roughly doubles it) |
| RDS gp3 storage, 50 GB | `db_allocated_storage_gb` | ~$6, doubled with Multi-AZ |
| Automated backups | 14 days retention | Free up to 100% of allocated storage, then ~$0.10/GB |
| Performance Insights | 7-day retention | Free at that tier |
| CloudWatch Logs (postgresql, upgrade) | always on | ~$0.50/GB ingested — usually a few dollars |
| Secrets Manager | 1 secret | ~$0.40 + API calls |
| ECR storage | 50 images × 4 repos | ~$0.10/GB-month; typically $2–10 |
| KMS customer-managed key | `kms_key_arn = null` | $0 by default; ~$1/key/month if you set one |
| IAM roles, SGs, subnet group | — | Free |
| S3 + DynamoDB remote state | — | Cents |

**Roughly $60–80/month** for the defaults, dominated by Multi-AZ RDS.

The two variables that actually move the number are `db_multi_az` (roughly
doubles instance *and* storage cost) and `db_instance_class`. Multi-AZ is on by
default because the alternative for a system of record is a failover measured
in however long a restore takes — but it is a real cost, and turning it off for
a staging environment is a reasonable decision to make deliberately.

### What this stack does **not** create, and costs much more

These are prerequisites you must already have, and they dwarf the above:

| Resource | Rough monthly |
|---|---|
| EKS control plane | ~$73 per cluster |
| Worker nodes (EC2) | From ~$30 for one small node; realistically $100+ |
| NAT gateway | ~$33 each, plus ~$0.045/GB processed |
| Application Load Balancer | ~$18 plus LCU charges |

A realistic all-in figure for a small production NEXUS installation is a few
hundred dollars a month, most of it the cluster and its networking rather than
anything in `modules/aws-managed`.

### Why it is opt-in

- The local path exists to be the default: full stack, no account, no bill,
  works on a plane.
- Nothing in `environments/local` references `modules/aws-managed`, so you
  cannot reach the AWS path by accident. `tofu init` in the local environment
  does not even download the AWS provider.
- RDS defaults here are durable rather than cheap — Multi-AZ, 14 days of
  backups, deletion protection on, final snapshot taken. Those are the right
  defaults for a system of record and the wrong ones for a five-minute
  experiment, which is another reason not to make this the default path.
- `deletion_protection` is on by default, so `tofu destroy` cannot take the
  database with it in one command. Turning it off is a separate, deliberate
  apply.

---

## Conventions

**No secrets anywhere in this tree.** The PostgreSQL password, JWT signing key
and webhook HMAC secret are generated with `random_password` and written only to
a Kubernetes Secret (and, on AWS, to Secrets Manager). No `.tfvars` file needs
to contain a credential, and `.gitignore` excludes `*.tfvars` anyway — the
`*.tfvars.example` templates are the checked-in documentation.

Credentials are never passed to Helm as values: anything handed to Helm is
stored in the release object in clear text, for every revision it keeps. The
chart reads a pre-existing Secret by name instead.

**State is sensitive.** It contains every generated credential in plain text.
`.gitignore` covers `*.tfstate*` and `*.tfplan`; a saved plan embeds resolved
values too.

**Every variable has a `description` and a `type`,** and a `validation` block
wherever a wrong value is catchable before apply — a `kube_context` that is not
a local cluster, an image tag of `latest`, an even NATS replica count that
cannot form a quorum, `0.0.0.0/0` in a database ingress rule.

---

## Verifying a change

```bash
cd infrastructure/tofu
tofu fmt -recursive
tofu fmt -recursive -check      # must exit 0

for env in local production; do
  (cd "environments/$env" && tofu init -backend=false && tofu validate)
done
```

`-backend=false` is what lets the production root be validated with no AWS
account and no state bucket. Both roots must pass on a machine with no
credentials at all; if `environments/production` ever needs credentials to
`validate`, something has been wired in that should have been a variable.
