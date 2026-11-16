# 3-Node NATS JetStream Cluster on Kubernetes

`values.yaml` (in this folder) deploys a 3-node clustered JetStream with **file
storage that survives pod restarts**, via the official Helm chart:

```bash
helm repo add nats https://nats-io.github.io/k8s/helm/charts/
helm repo update
# apply the StorageClass from the bottom of values.yaml first (or reuse an existing SSD class)
helm install nats nats/nats -f values.yaml -n nats-system --create-namespace
```

This gives you a StatefulSet of `nats-0`, `nats-1`, `nats-2`, each with its own
PersistentVolume for JetStream data.

## Why the data survives restarts

The durability comes from three layers, not one:

1. **File store on a PersistentVolume.** `jetstream.fileStore.enabled: true` makes
   the chart attach a PVC (StatefulSet `volumeClaimTemplate`) per pod. A PVC is
   **not** deleted when its pod restarts, reschedules, or is rolled during an
   upgrade — the pod re-binds the same volume and JetStream replays its store on
   boot. (The `memoryStore` is only for `R-mem` streams and is lost on restart;
   keep your durable streams on file storage.)
2. **R3 streams (replication).** Create your streams with `replicas: 3` so each
   message is written to all three nodes' volumes via Raft. A single PVC
   restart, or even losing a whole node, then loses no data — the other two
   replicas serve reads and re-sync the recovered node. File storage alone
   survives a *restart*; R3 survives a *node loss*. Set `replicas: 3` per stream
   when you create them (this is stream config, not Helm).
3. **3 nodes for quorum.** JetStream uses Raft; an odd node count (3) gives a
   majority for leader election. `podDisruptionBudget.maxUnavailable: 1` plus pod
   anti-affinity / topology spread keep two nodes (quorum) alive through node
   drains, upgrades, and single-zone failures.

The `reclaimPolicy: Retain` on the StorageClass is the backstop: even if a PVC is
ever deleted by accident, the underlying disk is kept, not wiped.

## Sizing

The values ship the **Medium** profile from the skill's guidelines, which is the
right default for most production workloads (10k–100k msg/s):

| Resource | Per node | Rationale |
|----------|----------|-----------|
| Nodes | 3 | Minimum for Raft quorum; odd number for leader election |
| CPU | request 2 / limit 4 | JetStream is write/fsync heavy; headroom for spikes |
| Memory | request 8Gi / limit 16Gi | Index + buffers; `memoryStore` capped at 2Gi |
| File store | 100Gi SSD/NVMe | Durable stream data; RWO, encrypted, expandable |

**Disk is the number to compute for your workload.** Estimate it, don't guess:

```
raw_retained = avg_msg_size × msg_rate_per_sec × retention_seconds
per_node     = raw_retained × replicas(3) / nodes(3)   # = raw_retained for R3
fileStore.size = per_node / 0.8                          # ~20% headroom for compaction/snapshots
```

Example: 2 KB msgs × 5,000/s × 24h retention ≈ 864 GB raw. With R3 across 3
nodes each node holds ~864 GB, so size the volume ~1.1 TB. The 100Gi default
fits lighter retention (e.g. 2 KB × 5k/s × ~2.5h). **Adjust `fileStore.size`
to your own retention before deploying.**

Scale the profile if your load differs:

- **Small (< 10k msg/s):** 3 nodes, 2 CPU, 4Gi RAM, ~50Gi disk.
- **Large (> 100k msg/s):** 5 nodes, 8+ CPU, 32+Gi RAM, 500Gi+ NVMe, 10Gbps net.

Rules of thumb baked into the values:
- `memoryStore.size` ≈ 25% of pod memory — leave the rest for OS, connections, index.
- `fileStore.size` is planned at ~80% usable (headroom for compaction/snapshots).
- Always SSD/NVMe. Spinning disks cause unacceptable fsync latency for JetStream.
- StorageClass is `ReadWriteOnce`, `allowVolumeExpansion: true`, SSD-backed, so you
  can grow a volume later without rebuilding the cluster.

## Operational safety included

- **Health probes** on `:8222` — readiness gates on `js-enabled-only`; the
  `startupProbe` allows ~7.5 min for store recovery on boot so a large store isn't
  killed mid-replay.
- **Pod anti-affinity + topology spread** — one pod per node, spread across zones
  when available, so a node or zone failure can't take two pods at once.
- **PodDisruptionBudget (maxUnavailable: 1)** — voluntary disruptions never break quorum.
- **Prometheus exporter** on `:7777` — scrape for `jetstream_disabled`, memory,
  and storage-fill alerts.
- **Pinned image** `2.14.6` (a supported line) — never `:latest` in production.

## Not included (out of scope for this request — add before go-live)

- **TLS / mTLS** for client and cluster traffic.
- **Authentication/authorization** (NKey or JWT/accounts, subject-level perms).
- **NetworkPolicy** to restrict who can reach `:4222` / `:6222` / `:8222`.
- **Backups** (JetStream snapshots) — replication is not a backup.

See the skill's `deployment/security.md` for TLS and auth, and
`deployment/kubernetes.md` for the NetworkPolicy manifest.
