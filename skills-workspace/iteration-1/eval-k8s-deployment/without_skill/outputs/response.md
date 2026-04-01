# Production-Grade NATS JetStream Cluster on Kubernetes

This guide covers deploying a 3-node NATS JetStream cluster using the official NATS Helm chart with:
- Persistent storage (PVC per pod)
- TLS (cert-manager issued certificates)
- NKey authentication
- Proper liveness/readiness/startup probes
- NetworkPolicy to restrict traffic

---

## Prerequisites

```bash
# Add the NATS Helm repo
helm repo add nats https://nats-io.github.io/k8s/helm/charts/
helm repo update

# cert-manager must be installed for TLS
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
```

---

## Step 1: Generate NKeys

NKeys use Ed25519 keypairs. Install the `nk` CLI tool and generate operator, account, and user keys.

```bash
# Install nk tool
go install github.com/nats-io/nkeys/nk@latest

# Generate operator NKey
nk -gen operator -pubout > operator.nk
OPERATOR_PUB=$(nk -inkey operator.nk -pubout)

# Generate account NKey
nk -gen account -pubout > account.nk
ACCOUNT_PUB=$(nk -inkey account.nk -pubout)

# Generate user NKey
nk -gen user -pubout > user.nk
USER_PUB=$(nk -inkey user.nk -pubout)

echo "Operator public key: $OPERATOR_PUB"
echo "Account public key:  $ACCOUNT_PUB"
echo "User public key:     $USER_PUB"
```

Store the private keys (seed files) securely — you will need the user seed to connect clients.

---

## Step 2: Create Namespace and Secrets

```bash
kubectl create namespace nats

# Store the NKey seeds as Kubernetes secrets
kubectl create secret generic nats-nkey-seeds \
  --namespace nats \
  --from-file=operator.nk=./operator.nk \
  --from-file=account.nk=./account.nk \
  --from-file=user.nk=./user.nk
```

---

## Step 3: TLS — Issuer and Certificate (cert-manager)

### `tls-issuer.yaml`

```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: nats-selfsigned-issuer
  namespace: nats
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nats-ca
  namespace: nats
spec:
  isCA: true
  commonName: nats-ca
  secretName: nats-ca-secret
  privateKey:
    algorithm: Ed25519
  issuerRef:
    name: nats-selfsigned-issuer
    kind: Issuer
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: nats-ca-issuer
  namespace: nats
spec:
  ca:
    secretName: nats-ca-secret
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nats-server-tls
  namespace: nats
spec:
  secretName: nats-server-tls
  commonName: nats
  duration: 8760h   # 1 year
  renewBefore: 720h # renew 30 days before expiry
  subject:
    organizations:
      - MyOrg
  dnsNames:
    - nats
    - nats.nats.svc
    - nats.nats.svc.cluster.local
    - "*.nats-headless.nats.svc.cluster.local"
    - "*.nats.nats.svc.cluster.local"
  privateKey:
    algorithm: RSA
    size: 4096
  issuerRef:
    name: nats-ca-issuer
    kind: Issuer
---
# Separate client-facing certificate (for mTLS if needed)
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nats-client-tls
  namespace: nats
spec:
  secretName: nats-client-tls
  commonName: nats-client
  duration: 8760h
  renewBefore: 720h
  dnsNames:
    - nats-client
  privateKey:
    algorithm: RSA
    size: 4096
  issuerRef:
    name: nats-ca-issuer
    kind: Issuer
```

Apply this before deploying the Helm chart:

```bash
kubectl apply -f tls-issuer.yaml
# Wait for certificates to be issued
kubectl wait --for=condition=Ready certificate/nats-server-tls -n nats --timeout=120s
```

---

## Step 4: NATS Resolver Config Secret

NATS JetStream with NKey auth uses a "full" or "memory" resolver. Below we use the `full` resolver backed by JetStream itself, which stores account JWTs in a JetStream stream.

You need to generate JWTs using the `nsc` tool for a full decentralised auth setup. The minimal example below uses a static account JWT approach with the `memory` resolver for simplicity while still requiring NKey-signed user credentials.

```bash
# Install nsc
curl -fsSL https://raw.githubusercontent.com/nats-io/nsc/main/install.sh | sh

# Bootstrap operator/account/user with nsc
nsc add operator myoperator
nsc add account myaccount
nsc add user myuser

# Export the resolver config
nsc generate config --mem-resolver --sys-account SYS > resolver.conf

# Export user credentials (used by clients)
nsc generate creds -n myuser -a myaccount > myuser.creds

# Store resolver config as a secret
kubectl create secret generic nats-resolver-config \
  --namespace nats \
  --from-file=resolver.conf=./resolver.conf
```

---

## Step 5: Helm values.yaml

This is the primary configuration file for the `nats/nats` Helm chart (v1.x / chart version aligned with NATS 2.10+).

### `values.yaml`

```yaml
# ============================================================
# NATS JetStream — Production 3-Node Cluster
# Helm chart: nats/nats (https://nats-io.github.io/k8s/)
# ============================================================

# ------------------------------------
# Global replica count — 3-node cluster
# ------------------------------------
replicaCount: 3

# ------------------------------------
# Image
# ------------------------------------
image:
  repository: nats
  tag: "2.10.14-alpine"
  pullPolicy: IfNotPresent

# ------------------------------------
# Pod settings
# ------------------------------------
podAnnotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "7777"
  cluster-autoscaler.kubernetes.io/safe-to-evict: "false"

podLabels:
  app.kubernetes.io/component: nats-server
  environment: production

# Spread pods across nodes/zones
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: nats
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: nats

affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchExpressions:
            - key: app.kubernetes.io/name
              operator: In
              values:
                - nats
        topologyKey: kubernetes.io/hostname

# ------------------------------------
# Security context
# ------------------------------------
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 1000
  runAsGroup: 1000
  fsGroup: 1000
  seccompProfile:
    type: RuntimeDefault

containerSecurityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop:
      - ALL

# ------------------------------------
# Resource requests and limits
# ------------------------------------
resources:
  requests:
    cpu: 250m
    memory: 512Mi
  limits:
    cpu: 2000m
    memory: 2Gi

# ------------------------------------
# Persistent storage for JetStream
# ------------------------------------
# The chart creates a PVC per pod via volumeClaimTemplates
nats:
  jetstream:
    enabled: true
    memStorage:
      enabled: true
      size: 1Gi
    fileStorage:
      enabled: true
      size: 20Gi
      storageClassName: "standard"    # Replace with your StorageClass
      accessModes:
        - ReadWriteOnce

# ------------------------------------
# Cluster configuration
# ------------------------------------
cluster:
  enabled: true
  replicas: 3
  # Cluster name must match across all nodes
  name: "nats-production"
  # TLS for inter-node (route) connections
  tls:
    enabled: true
    secretName: nats-server-tls
    # Mount path inside container
    cert: /etc/nats-certs/tls.crt
    key: /etc/nats-certs/tls.key
    ca: /etc/nats-certs/ca.crt

# ------------------------------------
# TLS for client connections
# ------------------------------------
tls:
  enabled: true
  secretName: nats-server-tls
  # Require clients to present a certificate (mTLS)
  # Set to false if you want TLS but not mutual TLS
  verify: false
  # Paths inside container
  cert: /etc/nats-certs/tls.crt
  key: /etc/nats-certs/tls.key
  ca: /etc/nats-certs/ca.crt

# ------------------------------------
# Authentication — NKey / JWT resolver
# ------------------------------------
auth:
  enabled: true
  # Use NATS decentralised auth (operator/account/user JWT + NKeys)
  resolver:
    enabled: true
    # Mount the resolver.conf from secret
    configSecret: nats-resolver-config
    configKey: resolver.conf

# ------------------------------------
# Extra volume mounts for TLS certs
# ------------------------------------
extraVolumes:
  - name: nats-tls
    secret:
      secretName: nats-server-tls

extraVolumeMounts:
  - name: nats-tls
    mountPath: /etc/nats-certs
    readOnly: true

# ------------------------------------
# Health probes
# ------------------------------------
# The NATS server exposes an HTTP monitoring endpoint on port 8222
liveness:
  httpGet:
    path: /healthz
    port: 8222
  initialDelaySeconds: 10
  periodSeconds: 30
  timeoutSeconds: 5
  failureThreshold: 3
  successThreshold: 1

readiness:
  httpGet:
    path: /readyz
    port: 8222
  initialDelaySeconds: 5
  periodSeconds: 10
  timeoutSeconds: 5
  failureThreshold: 3
  successThreshold: 1

startup:
  httpGet:
    path: /healthz
    port: 8222
  initialDelaySeconds: 5
  periodSeconds: 10
  timeoutSeconds: 5
  failureThreshold: 30   # Allow up to 5 minutes for startup
  successThreshold: 1

# ------------------------------------
# Service
# ------------------------------------
service:
  # ClusterIP is recommended; use LoadBalancer only if clients are external
  type: ClusterIP
  ports:
    client:
      enabled: true
      port: 4222
    cluster:
      enabled: true
      port: 6222
    monitor:
      enabled: true
      port: 8222
    metrics:
      enabled: true
      port: 7777

# ------------------------------------
# NATS Exporter (Prometheus metrics)
# ------------------------------------
exporter:
  enabled: true
  image:
    repository: natsio/prometheus-nats-exporter
    tag: "0.14.0"
  resources:
    requests:
      cpu: 50m
      memory: 64Mi
    limits:
      cpu: 200m
      memory: 128Mi
  args:
    - -connz
    - -routez
    - -subz
    - -varz
    - -jsz=all

# ServiceMonitor for Prometheus Operator
serviceMonitor:
  enabled: true
  namespace: monitoring    # Adjust to your Prometheus namespace
  labels:
    release: prometheus    # Must match your Prometheus Operator selector
  interval: 30s
  scrapeTimeout: 10s

# ------------------------------------
# Pod disruption budget
# ------------------------------------
podDisruptionBudget:
  enabled: true
  # Keep at least 2 nodes available during disruptions
  minAvailable: 2

# ------------------------------------
# Priority class (optional — create separately)
# ------------------------------------
priorityClassName: "high-priority"

# ------------------------------------
# NATS box (debugging sidecar) — disable in production
# ------------------------------------
natsbox:
  enabled: false

# ------------------------------------
# Config reloader sidecar
# ------------------------------------
reloader:
  enabled: true
  image:
    repository: natsio/nats-server-config-reloader
    tag: "0.14.1"
  resources:
    requests:
      cpu: 50m
      memory: 32Mi
    limits:
      cpu: 100m
      memory: 64Mi

# ------------------------------------
# Extra NATS server configuration
# Merged with chart-generated nats.conf
# ------------------------------------
extraConfig: |
  # Maximum payload size: 8MB
  max_payload: 8388608

  # Maximum number of pending bytes per subscription: 64MB
  max_pending_size: 67108864

  # Connection rate limit per second
  connect_error_reports: 5

  # Write deadline
  write_deadline: "10s"

  # Disable old-style authentication methods
  # (NKey/JWT is enforced via auth.resolver above)

  # JetStream limits
  jetstream {
    max_memory_store: 1073741824    # 1GB
    max_file_store:   21474836480   # 20GB
    # Store directory on the persistent volume
    store_dir: /data/jetstream
  }

  # Logging — structured JSON is easier to parse in most log aggregators
  # Use info level in production; debug adds significant overhead
  log_time: true
  log_file: ""
  debug: false
  trace: false
```

---

## Step 6: NetworkPolicy Manifests

### `networkpolicy.yaml`

```yaml
# ============================================================
# NetworkPolicy — NATS JetStream cluster
# Follows a default-deny posture; explicitly allow only what
# is needed.
# ============================================================

# --- Default deny all ingress and egress in the nats namespace ---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-all
  namespace: nats
spec:
  podSelector: {}
  policyTypes:
    - Ingress
    - Egress

---

# --- Allow NATS pods to receive client connections on port 4222 ---
# from pods carrying the label: nats-client: "true"
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-nats-client-ingress
  namespace: nats
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: nats
  policyTypes:
    - Ingress
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              # Permit any namespace labelled nats-clients: "true"
              nats-clients: "true"
          podSelector:
            matchLabels:
              nats-client: "true"
        # Also allow same-namespace clients (e.g. sidecars, batch jobs)
        - podSelector:
            matchLabels:
              nats-client: "true"
      ports:
        - protocol: TCP
          port: 4222

---

# --- Allow intra-cluster (route) traffic between NATS pods on port 6222 ---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-nats-cluster-routes
  namespace: nats
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: nats
  policyTypes:
    - Ingress
    - Egress
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: nats
      ports:
        - protocol: TCP
          port: 6222
  egress:
    - to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: nats
      ports:
        - protocol: TCP
          port: 6222

---

# --- Allow Prometheus scraping on port 7777 (metrics) ---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-prometheus-scrape
  namespace: nats
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: nats
  policyTypes:
    - Ingress
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
          podSelector:
            matchLabels:
              app.kubernetes.io/name: prometheus
      ports:
        - protocol: TCP
          port: 7777
        - protocol: TCP
          port: 8222   # monitoring HTTP endpoint

---

# --- Allow egress to kube-dns (required for cluster DNS resolution) ---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-nats-egress-dns
  namespace: nats
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: nats
  policyTypes:
    - Egress
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53

---

# --- Allow NATS pods to reach the Kubernetes API server ---
# Needed if using the NATS Operator or cert-manager webhooks
# Remove this if your setup does not require API server access.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-nats-egress-apiserver
  namespace: nats
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: nats
  policyTypes:
    - Egress
  egress:
    - to:
        - ipBlock:
            # Replace with your API server CIDR if different
            cidr: 0.0.0.0/0
            except:
              - 10.0.0.0/8
              - 172.16.0.0/12
              - 192.168.0.0/16
      ports:
        - protocol: TCP
          port: 443
        - protocol: TCP
          port: 6443
```

---

## Step 7: Supporting Manifests

### `priorityclass.yaml`

```yaml
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: high-priority
value: 1000000
globalDefault: false
description: "Priority class for production NATS JetStream cluster"
```

### `storageclass.yaml` (if needed)

```yaml
# Only create this if your cluster does not already have a suitable StorageClass.
# This example is for AWS EBS gp3.
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nats-storage
provisioner: ebs.csi.aws.com
parameters:
  type: gp3
  iops: "3000"
  throughput: "125"
  encrypted: "true"
reclaimPolicy: Retain          # Retain data even if PVC is deleted
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

Update `values.yaml` to use `storageClassName: "nats-storage"` if you deploy this StorageClass.

---

## Step 8: Deploy

```bash
# 1. Apply supporting manifests
kubectl apply -f priorityclass.yaml
kubectl apply -f tls-issuer.yaml
kubectl apply -f networkpolicy.yaml

# 2. Wait for cert-manager to issue the certificate
kubectl wait --for=condition=Ready certificate/nats-server-tls -n nats --timeout=120s

# 3. Install the Helm chart
helm install nats nats/nats \
  --namespace nats \
  --create-namespace \
  --values values.yaml \
  --wait \
  --timeout 10m

# 4. Verify the deployment
kubectl rollout status statefulset/nats -n nats
kubectl get pods -n nats -o wide
kubectl exec -n nats nats-0 -- nats-server --version
```

---

## Step 9: Connecting Clients

Clients must:
1. Trust the CA certificate (`nats-ca-secret`).
2. Present a valid user credential file (`.creds`) signed by your operator.

```bash
# Extract the CA cert for clients
kubectl get secret nats-ca-secret -n nats \
  -o jsonpath='{.data.ca\.crt}' | base64 -d > ca.crt

# Example: connect with the nats CLI using creds + TLS
nats context add production \
  --server tls://nats.nats.svc.cluster.local:4222 \
  --creds ./myuser.creds \
  --tlsca ./ca.crt

nats context select production
nats pub test.subject "hello world"
```

---

## Security Hardening Summary

| Control | Implementation |
|---|---|
| Encryption in transit | TLS 1.2+ on client (4222) and route (6222) ports via cert-manager issued certs |
| Authentication | NKey-based JWT decentralised auth (operator → account → user chain) |
| Authorization | Per-account and per-user subject permissions in JWTs |
| Network segmentation | NetworkPolicy: default-deny, allow-listed ingress/egress per port |
| Pod security | Non-root user (1000), read-only rootfs, all capabilities dropped, seccomp RuntimeDefault |
| Storage encryption | StorageClass `encrypted: "true"` (provider-dependent) |
| Availability | PodAntiAffinity (hard), TopologySpreadConstraints, PodDisruptionBudget (minAvailable: 2) |
| Metrics | Prometheus exporter on port 7777, ServiceMonitor for auto-discovery |
| Config reload | `nats-server-config-reloader` sidecar — no restarts needed for config changes |

---

## Notes and Production Checklist

- Replace `storageClassName: "standard"` with your actual StorageClass (e.g. `gp3`, `premium-rwo`).
- Replace the `monitoring` namespace label in the NetworkPolicy with your actual Prometheus namespace.
- The `nsc`-based resolver config (`resolver.conf`) must be regenerated whenever you add new accounts or users.
- Rotate TLS certificates before expiry — cert-manager handles this automatically with the `renewBefore` field.
- Consider using a `full` JetStream resolver (backed by a JetStream stream) instead of `memory` for larger deployments with many accounts.
- Enable NATS monitoring dashboard via Grafana using the official [NATS Grafana dashboards](https://grafana.com/grafana/dashboards/?search=nats).
- For leaf node or gateway federation, additional cluster/gateway blocks and corresponding NetworkPolicy rules are required.
