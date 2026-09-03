# Screenshot capture guide

Six images. The pipeline run is the hero — if only one gets captured, capture that one.

No placeholder files are committed. A blank PNG in a portfolio is worse than a missing
one, so the README references these paths and they appear once the files exist.

---

## Before you start

**1. Bring the stack up with the Kubernetes provider**, so the console reports a real
cluster rather than the Docker fallback:

```powershell
.\scripts\kind.ps1 bootstrap

$env:NEXUS_DEPLOY_PROVIDER = 'kubernetes'
$env:NEXUS_KUBE_CONTEXT    = 'kind-nexus-local'
$env:NEXUS_KIND_CLUSTER    = 'nexus-local'
.\scripts\dev.ps1 up
```

**2. Confirm the estate looks healthy** before capturing anything:

```powershell
.\bin\nexus.exe status
```

Expect roughly 3 projects, 10 environments, a success rate in the eighties, 2 live
workers, queue depth 0 and no degraded deployments. If the queue is not empty or a run is
still in flight, wait — a half-finished run reads as a broken system rather than a busy
one.

**3. Sign in** at http://localhost:3100 as `admin@nexus.local` / `nexus-demo-password`.
Admin, so every action is visible rather than hidden by role.

---

## Capture settings

| | |
|---|---|
| Viewport | **1512 × 945** |
| Browser chrome | **Hidden.** Capture the page only — no tabs, no address bar, no bookmarks |
| Zoom | 100% |
| Theme | Whichever you prefer, but use **the same one for all six** |
| Format | PNG |
| Location | `docs/screenshots/` |

The quickest way to get an exact viewport with no chrome: open DevTools (F12) → device
toolbar (Ctrl+Shift+M) → set **Responsive** to 1512 × 945 → ⋮ menu → **Capture
screenshot**.

Before each capture: scroll to the top, move the mouse off the page so no hover state or
tooltip is showing, and let any loading spinner finish.

---

## 01 — Overview

| | |
|---|---|
| **URL** | http://localhost:3100/console |
| **Filename** | `01-overview.png` |
| **Shows** | Healthy system at a glance |

**State to prepare:** nothing in flight. Queue depth 0, no running runs.

**What must be visible:** the metric tiles (projects, success rate, deployments, workers
live), recent pipeline runs with a believable mix of succeeded and failed, recent
deployments, and the worker list showing both workers active.

**Scroll position:** top. Capture the tiles and at least the first few recent-activity
rows.

---

## 02 — Pipeline run *(hero image)*

| | |
|---|---|
| **URL** | http://localhost:3100/console/pipelines/`<run-id>` |
| **Filename** | `02-pipeline-run.png` |
| **Shows** | A complete successful run, including a real container build |

**Which run:** the most recent successful one that performed a genuine container build.
In the current database that is **run #15**:

```
http://localhost:3100/console/pipelines/run_01m1kmabcxxfhvamcgvxw5m2bm
```

If the database has been reseeded, find the newest succeeded run:

```powershell
.\bin\nexus.exe pipeline list | Select-Object -First 5
```

Or trigger a fresh one, which takes about 25 seconds:

```powershell
.\bin\nexus.exe pipeline run prj_01m1jvhyvzdxhxq4q2rchgz4pr
.\bin\nexus.exe pipeline watch <run-id>
```

**What must be visible:** the commit and branch, total duration, every stage with its own
duration, and — most importantly — **`containerise` showing a duration in the tens of
seconds**. That single number is what tells a reader the build is real rather than
simulated. Also visible: the worker that executed each job, and the deploy and
verification steps after the build.

**Expand:** the stage list, so all nine steps are shown. If a step's log panel can be
opened without pushing the stage list off screen, opening `containerise` to reveal Docker
build output is worth it.

---

## 03 — Deployment

| | |
|---|---|
| **URL** | http://localhost:3100/console/deployments/`<deployment-id>` |
| **Filename** | `03-deployment.png` |
| **Shows** | A healthy deployment with the evidence behind it |

**Which deployment:** the current healthy staging deployment. In the current database:

```
http://localhost:3100/console/deployments/dep_01m1kkj7en8jkhz6ty5625sv6z
```

**What must be visible:**

- environment and project
- the image reference **and its digest**
- the deploy provider reading `kubernetes`
- desired revision and observed revision, **matching**
- health status HEALTHY
- the health-check table with its consecutive passing probes, status codes and latencies
- the deployment timeline

The health-check rows are the point of this screenshot. They are the difference between
"we ran an apply" and "we watched it serve".

---

## 04 — Infrastructure

| | |
|---|---|
| **URL** | http://localhost:3100/console/infrastructure |
| **Filename** | `04-infrastructure.png` |
| **Shows** | The platform underneath |

**State to prepare:** both workers running and registered. If a worker was killed for the
lease demonstration, restart it and wait for its heartbeat.

**What must be visible:** the deploy provider (`kubernetes`), worker rows with
heartbeat/liveness and slot usage (`0/4` idle or `n/4` busy), PostgreSQL and NATS status,
the reconciler, and queue activity.

**Optional but stronger:** trigger a run first and capture while workers are mid-job, so
slots read `2/4` or `4/4` rather than idle. A busy platform photographs better than an
empty one — but only if nothing is failing.

---

## 05 — Observability

| | |
|---|---|
| **URL** | http://localhost:3100/console/observability |
| **Filename** | `05-observability.png` |
| **Shows** | The system measuring itself |

**State to prepare:** run a few pipelines first so the charts have shape. Flat lines at
zero make the page look unimplemented:

```powershell
1..3 | ForEach-Object { .\bin\nexus.exe pipeline run prj_01m1jvhyvzdxhxq4q2rchgz4pr }
```

Wait for them to finish, then capture.

**What must be visible:** pipeline duration and outcome metrics, job metrics including
retries, deployment metrics, error rate, queue depth and worker utilisation.

**Alternative if the console page is thinner than the underlying data:** Grafana at
http://localhost:3300 shows the same series with more visual weight. Either is honest —
both read real Prometheus data — so pick whichever page is actually stronger.

---

## 06 — Failure and rollback

| | |
|---|---|
| **URL** | http://localhost:3100/console/deployments |
| **Filename** | `06-failure-rollback.png` |
| **Shows** | Bad release → failed health → rollback → recovery |

**State to prepare:** the current database already contains the full sequence on Checkout
API staging:

```
#20  HEALTHY       r13@manual0       ← the rollback, healthy again
#19  ROLLED_BACK   r99-bad@deadbee   ← the deliberately broken release
#18  SUPERSEDED    r13@manual0       ← the last good release
```

Filter or scroll the deployments list so **all three rows are visible together**. The
story only reads if the eye can follow it down the list.

To recreate it from scratch:

```powershell
.\scripts\demo failure
.\bin\nexus.exe deployment rollback env_01m1jvhyydceqr9jzxq0678w61
```

**Alternative framing, if the list view is cramped:** capture the failed deployment's own
page instead —

```
http://localhost:3100/console/deployments/dep_01m1kkdsjhrwk40hd8ngxscme6
```

— which shows the FAILED state with the cause attached and the rollback that superseded
it. The detail page tells the story more clearly; the list view shows it as a sequence.
Capture whichever reads better at 1512 × 945.

---

## After capturing

```powershell
Get-ChildItem docs\screenshots\*.png | Select-Object Name, Length
```

Check each file is a real screenshot at the right size, then commit them. The README
already references these exact filenames, so no edit is needed once they exist.
