# Screenshots

Captured at **1512 x 945**, which is the width the console's ledgers are laid out for —
narrower captures wrap the readout row onto two lines and the columns stop aligning.

Sign in as `admin@nexus.local` before capturing, and have the stack seeded so the
estate is populated rather than empty.

| File | Page | What should be on screen |
|---|---|---|
| `01-overview.png` | `/console` | The readout row, recent runs interleaved across all three projects with at least one FAILED among them, and the environment grid |
| `02-pipeline-run.png` | `/console/pipelines/<run-id>` | A successful run with the execution lane showing all nine steps across its waves, a step selected, and its output and attempt history in the inspector |
| `03-deployment.png` | `/console/deployments/<deployment-id>` | A Kubernetes deployment: desired against observed state side by side, the real image and digest, and the health-check table |
| `04-observability.png` | `/console/observability` | The exported metric series and the Prometheus query block |
| `05-infrastructure.png` | `/console/infrastructure` | Live workers with heartbeats, capacity, and the deploy provider reported as `kubernetes` |

The pipeline run is the primary image. It is the screen that shows the most engineering
at once: DAG structure, concurrency, per-step timing, attempt history and logs.
