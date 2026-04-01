# NATS JetStream 3-Node Cluster on Kubernetes — Production Deployment

This guide covers a full production-grade NATS JetStream cluster deployment using the official Helm chart, including persistent storage, TLS, NKey authentication, health probes, and NetworkPolicy.

---

## Prerequisites

```bash
helm repo add nats https://nats-io.github.io/k8s/helm/charts/
helm repo update
```

---

## 1. TLS Certificates

Generate certificates before deploying. For production, use your internal PKI or cert-manager. The example below uses OpenSSL.

```bash
# Generate CA
openssl genrsa -out ca-key.pem 4096
openssl req -new -x509 -key ca-key.pem -out ca.pem -days 365 \
  -subj "/CN=NATS CA"

# Generate server cert (covers all pod DNS names + headless service)
openssl genrsa -out server-key.pem 2048
openssl req -new -key server-key.pem -out server.csr \
  -subj "/CN=nats-server"
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out server-cert.pem -days 365 \
  -extfile <(echo "subjectAltName=DNS:nats-0.nats.nats-system.svc.cluster.local,DNS:nats-1.nats.nats-system.svc.cluster.local,DNS:nats-2.nats.nats-system.svc.cluster.local,DNS:nats.nats-system.svc.cluster.local,DNS:localhost")

# Generate cluster route cert (used for inter-node TLS)
openssl genrsa -out cluster-key.pem 2048
openssl req -new -key cluster-key.pem -out cluster.csr \
  -subj "/CN=nats-cluster"
openssl x509 -req -in cluster.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out cluster-cert.pem -days 365 \
  -extfile <(echo "subjectAltName=DNS:nats-0.nats.nats-system.svc.cluster.local,DNS:nats-1.nats.nats-system.svc.cluster.local,DNS:nats-2.nats.nats-system.svc.cluster.local")
```

Store the certs in a Kubernetes Secret:

```bash
kubectl create namespace nats-system

kubectl create secret generic nats-tls \
  --from-file=ca.pem \
  --from-file=server-cert.pem \
  --from-file=server-key.pem \
  --from-file=cluster-cert.pem \
  --from-file=cluster-key.pem \
  -n nats-system
```

---

## 2. NKey Authentication

Generate NKey pairs for each service that will connect. The server only stores public keys.

```bash
# Install the nk tool
go install github.com/nats-io/nkeys/nk@latest

# Generate a key pair for each application user
nk -gen user -pubout   # run once per user; outputs seed (private) + public key

# Example output for order-service:
#   SUAM7BQBSMZEXAMPLEKEYHERE...   (seed — keep secret, put in app Secret)
#   UA7BQBSMZEXAMPLEPUBKEYHERE...  (public key — put in server config)
```

Store seeds as Kubernetes Secrets in the application namespaces (not the nats-system namespace):

```bash
kubectl create secret generic nats-nkey-order-service \
  --from-literal=nkey.seed=SUAM7BQBSMZ... \
  -n order-service
```

The server config (rendered via the Helm chart's `nats.config` or extra config blocks) references only the public keys — seeds never leave the application pods.

---

## 3. values.yaml

```yaml
# values.yaml — NATS JetStream 3-node production cluster
# Helm chart: nats/nats

nats:
  image:
    tag: "2.10.24"
    pullPolicy: IfNotPresent

  # JetStream storage configuration
  jetstream:
    enabled: true
    memoryStore:
      enabled: true
      size: 4Gi          # matches max_mem in server config
    fileStore:
      enabled: true
      size: 100Gi
      storageClassName: gp3-encrypted   # replace with your StorageClass

  # Connection and payload limits
  limits:
    maxConnections: 64000
    maxPayload: 8MB
    maxPending: 64MB

  # Pod resource requests/limits
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

  # TLS for client connections (server TLS with mTLS enabled)
  tls:
    secret:
      name: nats-tls
    ca: "ca.pem"
    cert: "server-cert.pem"
    key: "server-key.pem"
    verify: true          # enable mTLS — clients must present a cert

  # Extra server configuration for NKey auth and JetStream domain
  # The Helm chart merges this into the generated nats-server.conf
  config:
    jetstream:
      domain: production

    authorization:
      users:
        # order-service NKey (replace with your actual public key)
        - nkey: "UA7BQBSMZORDER000EXAMPLEPUBKEY"
          permissions:
            publish:
              allow:
                - "orders.>"
                - "$JS.API.>"
            subscribe:
              allow:
                - "orders.>"
                - "_INBOX.>"
        # analytics-service NKey
        - nkey: "UB3DKQMSLANALYTICS000EXAMPLEPUBKEY"
          permissions:
            publish:
              deny:
                - ">"
            subscribe:
              allow:
                - "orders.>"
                - "payments.>"
                - "_INBOX.>"
        # admin NKey (for ops tooling)
        - nkey: "UCADMIN000EXAMPLEPUBKEY"

  # Health probes using NATS monitoring port
  readinessProbe:
    httpGet:
      path: /healthz?js-enabled-only=true
      port: 8222
    initialDelaySeconds: 10
    periodSeconds: 10
    timeoutSeconds: 5
    failureThreshold: 3

  livenessProbe:
    httpGet:
      path: /healthz
      port: 8222
    initialDelaySeconds: 10
    periodSeconds: 30
    timeoutSeconds: 5
    failureThreshold: 3

  startupProbe:
    httpGet:
      path: /healthz
      port: 8222
    initialDelaySeconds: 5
    periodSeconds: 5
    failureThreshold: 90   # up to 7.5 min for large JetStream restore on startup

# 3-node cluster
cluster:
  enabled: true
  replicas: 3
  noAdvertise: false
  name: "nats-cluster"

  # TLS for inter-node cluster routes
  tls:
    secret:
      name: nats-tls
    ca: "ca.pem"
    cert: "cluster-cert.pem"
    key: "cluster-key.pem"
    verify: true

  # Cluster route connect retries — allows nodes to boot in any order
  extraConfig: |
    connect_retries: 120

# Spread pods across nodes (required for HA)
affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/name: nats
        topologyKey: kubernetes.io/hostname

# Optional: spread across availability zones if you have multiple zones
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: nats

# Prevent eviction below quorum — never lose more than 1 pod at a time
podDisruptionBudget:
  enabled: true
  maxUnavailable: 1

# nats-box sidecar for debugging/operations
natsBox:
  enabled: true

# Prometheus exporter sidecar
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

---

## 4. NetworkPolicy Manifest

This policy locks down the NATS pods so only explicitly labelled namespaces can connect.

```yaml
# networkpolicy.yaml
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
    # Allow client connections from namespaces labelled nats-client=true
    - ports:
        - port: 4222
          protocol: TCP
      from:
        - namespaceSelector:
            matchLabels:
              nats-client: "true"

    # Allow cluster route traffic between NATS pods only
    - ports:
        - port: 6222
          protocol: TCP
      from:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: nats

    # Allow monitoring scrapes from Prometheus namespace
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
    # Allow outbound cluster route traffic to peer NATS pods
    - ports:
        - port: 6222
          protocol: TCP
      to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: nats

    # Allow DNS resolution (required for pod startup and cluster routes)
    - ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
```

Label your application namespaces to grant client access:

```bash
# Allow the order-service namespace to connect to NATS
kubectl label namespace order-service nats-client=true

# Allow the analytics namespace to connect
kubectl label namespace analytics nats-client=true

# Label the Prometheus namespace for monitoring access
kubectl label namespace monitoring monitoring=true
```

---

## 5. Install

```bash
# Create namespace and TLS secret first (see Section 1)
kubectl create namespace nats-system

kubectl create secret generic nats-tls \
  --from-file=ca.pem \
  --from-file=server-cert.pem \
  --from-file=server-key.pem \
  --from-file=cluster-cert.pem \
  --from-file=cluster-key.pem \
  -n nats-system

# Deploy the Helm chart
helm install nats nats/nats \
  -f values.yaml \
  -n nats-system

# Apply the NetworkPolicy
kubectl apply -f networkpolicy.yaml
```

---

## 6. Validation

```bash
# Check pod status — all 3 should be Running
kubectl get pods -n nats-system

# Check JetStream is enabled on all nodes
kubectl exec -n nats-system nats-nats-box-0 -- \
  nats server info --server nats://nats.nats-system.svc.cluster.local:4222

# Check cluster formation (should show 3 nodes)
kubectl exec -n nats-system nats-nats-box-0 -- \
  nats server list

# Verify JetStream
kubectl exec -n nats-system nats-nats-box-0 -- \
  nats account info

# Check health endpoint directly
kubectl exec -n nats-system nats-0 -- \
  curl -s http://localhost:8222/healthz?js-enabled-only=true
```

---

## Key Design Notes

**Storage**: Each pod gets its own PVC via the StatefulSet volume claim template. The `gp3-encrypted` StorageClass should support `ReadWriteOnce`, volume expansion, and SSDs. Replace with your cluster's equivalent (e.g., `pd-ssd` on GKE, `managed-premium` on AKS).

**TLS**: Server TLS is configured with `verify: true` (mTLS), meaning clients must present a certificate signed by the CA. Cluster routes also use TLS with mutual verification. If your clients cannot present certs, remove `verify: true` from the client TLS block to fall back to one-way TLS.

**NKey auth**: Only public keys are stored in the server config. Seeds (private keys) are stored in application-namespace Secrets and mounted into app pods. Applications use the NKey seed to sign challenge responses — the private key never leaves the pod.

**Pod anti-affinity**: The `requiredDuringSchedulingIgnoredDuringExecution` rule enforces that no two NATS pods land on the same Kubernetes node. This is required for meaningful HA — if it can't be satisfied, pods will remain Pending. If your cluster has fewer than 3 nodes, switch to `preferredDuringSchedulingIgnoredDuringExecution`.

**connect_retries: 120**: Allows cluster formation even when pods boot in different orders. At a 1-second retry interval this gives 2 minutes for all nodes to become reachable.

**PodDisruptionBudget**: `maxUnavailable: 1` ensures that cluster operations (node drains, rolling upgrades) never take down more than one NATS pod at a time, preserving the R=3 quorum.

**Health probes**: The readiness probe uses `?js-enabled-only=true` which returns non-200 until JetStream is fully initialized (including stream recovery from disk). This prevents a pod from receiving client traffic before it is ready. The startup probe has `failureThreshold: 90` (7.5 minutes) to accommodate large JetStream store recovery on restart.
