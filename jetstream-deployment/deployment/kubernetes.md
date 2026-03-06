# Kubernetes Deployment

## Helm Chart (Recommended)

```bash
helm repo add nats https://nats-io.github.io/k8s/helm/charts/
helm repo update
```

### Production values.yaml

```yaml
nats:
  image:
    tag: "2.10.24"
    pullPolicy: IfNotPresent

  jetstream:
    enabled: true
    memoryStore:
      enabled: true
      size: 4Gi
    fileStore:
      enabled: true
      size: 100Gi
      storageClassName: gp3-encrypted

  limits:
    maxConnections: 64000
    maxPayload: 8MB
    maxPending: 64MB

  resources:
    requests:
      cpu: "2"
      memory: 8Gi
    limits:
      cpu: "4"
      memory: 16Gi

  logging:
    debug: false
    trace: false
    logtime: true

cluster:
  enabled: true
  replicas: 3
  noAdvertise: false

natsBox:
  enabled: true

promExporter:
  enabled: true
  port: 7777

podDisruptionBudget:
  enabled: true
  maxUnavailable: 1
```

### Install

```bash
helm install nats nats/nats -f values.yaml -n nats-system --create-namespace
```

## StatefulSet Considerations

The Helm chart creates a StatefulSet:

- Pods are named `nats-0`, `nats-1`, `nats-2` — stable network identities
- Each pod gets a PersistentVolumeClaim for JetStream storage
- Rolling updates replace one pod at a time (preserving quorum)
- Pod anti-affinity ensures pods schedule on different nodes

### Pod Anti-Affinity

```yaml
affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/name: nats
        topologyKey: kubernetes.io/hostname
```

### Topology Spread (multi-zone)

```yaml
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: nats
```

## PersistentVolumeClaim

```yaml
# Ensure StorageClass supports:
# - ReadWriteOnce access mode
# - Volume expansion (allowVolumeExpansion: true)
# - Fast SSD/NVMe backing (io1, gp3, pd-ssd)

volumeClaimTemplates:
  - metadata:
      name: nats-js
    spec:
      accessModes: ["ReadWriteOnce"]
      storageClassName: gp3-encrypted
      resources:
        requests:
          storage: 100Gi
```

## Health Checks

NATS exposes health endpoints on monitoring port 8222:

```yaml
readinessProbe:
  httpGet:
    path: /healthz?js-enabled-only=true
    port: 8222
  initialDelaySeconds: 10
  periodSeconds: 10
  timeoutSeconds: 5

livenessProbe:
  httpGet:
    path: /healthz
    port: 8222
  initialDelaySeconds: 10
  periodSeconds: 30
  timeoutSeconds: 5

startupProbe:
  httpGet:
    path: /healthz
    port: 8222
  initialDelaySeconds: 5
  periodSeconds: 5
  failureThreshold: 90   # 7.5 min for large JetStream stores
```

## NetworkPolicy

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: nats-network-policy
  namespace: nats-system
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: nats
  policyTypes:
    - Ingress
    - Egress
  ingress:
    # Client connections
    - ports:
        - port: 4222
          protocol: TCP
      from:
        - namespaceSelector:
            matchLabels:
              nats-client: "true"
    # Cluster routes
    - ports:
        - port: 6222
          protocol: TCP
      from:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: nats
    # Monitoring
    - ports:
        - port: 8222
          protocol: TCP
        - port: 7777
          protocol: TCP
      from:
        - namespaceSelector:
            matchLabels:
              monitoring: "true"
  egress:
    - ports:
        - port: 6222
          protocol: TCP
      to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: nats
    - ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
```

## Prometheus Monitoring

```yaml
promExporter:
  enabled: true
  port: 7777
  image:
    tag: "0.15.0"

serviceMonitor:
  enabled: true
  namespace: monitoring
  labels:
    release: prometheus
```

### Key Metrics to Alert On

| Metric | Threshold | Meaning |
|--------|-----------|---------|
| `gnatsd_varz_jetstream_disabled` | = 1 | JetStream disabled on node |
| `gnatsd_connz_total` | > 80% of max_connections | Connection limit approaching |
| `gnatsd_varz_mem` | > 80% of pod memory limit | Memory pressure |
| `gnatsd_varz_jetstream_stats_storage` | > 80% of max_file | Storage filling up |

## Client Connection from Pods

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: order-service
spec:
  template:
    spec:
      containers:
        - name: order-service
          env:
            - name: NATS_URL
              value: "nats://nats.nats-system.svc.cluster.local:4222"
```
