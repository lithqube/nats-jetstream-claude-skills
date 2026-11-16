# 3-node NATS JetStream cluster on Kubernetes (production, persistent file storage)

## What to deploy

Use the **official `nats` Helm chart**, not a hand-written StatefulSet. The chart
already produces everything a production cluster needs — a clustered StatefulSet,
headless service for peer discovery, **per-pod PersistentVolumeClaims**, a
PodDisruptionBudget, pod anti-affinity, and a config-reloader sidecar — so the
only thing you own is `values.yaml`.

```bash
helm repo add nats https://nats-io.github.io/k8s/helm/charts/
helm repo update
helm upgrade --install nats nats/nats -n nats --create-namespace -f values.yaml
```

`values.yaml` (in this folder) turns on clustering (`replicas: 3`), JetStream,
and a file-store PVC.

## Why this survives pod restarts

The chart renders `jetstream.fileStore.pvc` as a **`volumeClaimTemplate`** on the
StatefulSet. Each pod (`nats-0`, `nats-1`, `nats-2`) therefore gets its own stable
PVC/PV that is **re-bound to the same pod by ordinal** on any restart, reschedule,
or rolling upgrade. Stream data lives on that volume (`/data`), so it is retained.
The things that do **not** survive a restart are memory-store streams — so keep any
data you care about on file storage (`storage: file` when you create the stream),
which is the default.

One caveat: deleting the StatefulSet does **not** delete the PVCs (good — your data
is safe), but it also means you must clean them up manually if you ever intend a
truly fresh install.

## Sizing rationale

**Node count — why 3.** JetStream replication uses RAFT, which needs a majority
quorum. 3 nodes give a quorum of 2, so the cluster keeps accepting writes with **1
node down** (maintenance or failure). 1 node = no HA; 2 nodes = no quorum when one
is lost; 3 is the smallest real HA size. Go to 5 only if you must tolerate 2
simultaneous losses. Create R3 streams (`replicas: 3`) so each stream has a copy on
every node.

**Per-pod resources (starting points, tune from real metrics):**

| Resource | Request | Limit | Why |
|---|---|---|---|
| CPU | 2 | (none) | Fan-out + RAFT replication are CPU-bound; no limit avoids CFS throttling under burst. |
| Memory | 4Gi | 8Gi | RAM holds the write/ack cache, RAFT state, and in-flight messages. Cap it to protect the node. |
| Disk (PVC) | 50Gi SSD | — | Sized from retention, not throughput (see below). |

**Disk sizing.** Size the PVC from how much data you retain, then add headroom:

```
PVC per pod ≈ (avg msg size × msgs/sec × retention seconds) × replication_overhead × 1.5 headroom
```

- With R3 streams, the *same* data lands on all 3 nodes, so each pod's PVC must hold
  the full retained set — not one-third of it.
- Add ~1.5× headroom for index/overhead and so you have room to react before a disk
  fills. A full JetStream disk stalls writes.
- Set stream **limits** (`MaxBytes` / `MaxAge`) below the PVC size so retention is
  bounded by policy, not by running out of disk. Example: 100 msg/s × 1KB × 7-day
  retention ≈ 60GB raw → 50-100Gi PVC per pod is a reasonable band. Measure and
  resize (SSD classes like gp3 allow online volume expansion).

**Storage class.** Use a real block/SSD class with `ReadWriteOnce` (gp3 on EKS,
pd-ssd/premium-rwo on GKE, managed-csi on AKS). Avoid network filesystems (NFS/EFS)
for the JetStream store — fsync latency and file-locking semantics cause corruption
and poor performance.

**Placement.** Spread the 3 pods across nodes (chart default anti-affinity) and
across availability zones (the `topologySpreadConstraints` in `values.yaml`) so a
single node or AZ failure removes at most one RAFT member.

## Before you call it production

- Pin the chart version (`--version`) and the server image tag (`config.image.tag`).
- Add **TLS** and **auth** (NKey/JWT or at least token/user accounts) — the values
  here omit them for brevity; an unauthenticated JetStream cluster should never face
  untrusted networks.
- Scrape `/healthz` and `/varz` on `:8222` (probes are already wired); enable
  `promExporter` if you run Prometheus.

---
Skipped / not checked: TLS, authn/authz, and multi-region gateways are called out
but not configured — add them before exposing the cluster. Exact field paths and
defaults track the `nats` chart v1.x schema; confirm against the version you pin
(`helm show values nats/nats`), since older/newer chart versions move a few keys.
The sizing numbers are formulas with one worked example, not measurements — size
from your own throughput and retention.
