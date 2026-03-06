# Cluster Setup

## Single-Node Development

Minimal nats-server.conf for local development with JetStream:

```conf
# nats-server.conf (development)
listen: 0.0.0.0:4222
http: 0.0.0.0:8222

jetstream {
  store_dir: /data/jetstream
  max_mem: 1G
  max_file: 10G
}
```

## 3-Node Production Cluster

### Node 1 (nats-1.conf)

```conf
server_name: nats-1
listen: 0.0.0.0:4222
http: 0.0.0.0:8222

jetstream {
  server_name: nats-1
  store_dir: /data/jetstream
  max_mem: 4G
  max_file: 100G
  domain: production
}

cluster {
  name: nats-cluster
  listen: 0.0.0.0:6222

  routes: [
    nats-route://nats-1:6222
    nats-route://nats-2:6222
    nats-route://nats-3:6222
  ]

  connect_retries: 120
}
```

### Node 2 and Node 3

Identical configuration except `server_name` changes to `nats-2` / `nats-3`.

### Key Cluster Settings

| Setting | Recommendation | Notes |
|---------|---------------|-------|
| `cluster.name` | Same across all nodes | Required for cluster formation |
| `routes` | List all nodes including self | NATS deduplicates self-routes |
| `connect_retries` | 120 (2 min at 1s interval) | Allows nodes to boot in any order |
| `jetstream.domain` | Set for super-cluster isolation | Optional for single cluster |
| `jetstream.max_mem` | 25-50% of system RAM | Leave headroom for OS and connections |
| `jetstream.max_file` | 80% of dedicated volume | Leave room for compaction/snapshots |

## Cluster Sizing Guidelines

### Small (< 10k msgs/sec)
- 3 nodes
- 2 CPU cores per node
- 4 GB RAM per node
- 50 GB SSD per node

### Medium (10k-100k msgs/sec)
- 3-5 nodes
- 4 CPU cores per node
- 8-16 GB RAM per node
- 200 GB NVMe SSD per node

### Large (> 100k msgs/sec)
- 5 nodes
- 8+ CPU cores per node
- 32+ GB RAM per node
- 500 GB+ NVMe SSD per node
- Dedicated network (10 Gbps+)

### Disk Selection
- Always use SSDs — spinning disks cause unacceptable write latency
- NVMe preferred for high-throughput workloads
- Dedicated volumes — never share JetStream storage with OS
- Use `ext4` or `xfs` filesystem
- Disable `atime` mount option for write performance

## Multi-Region / Super-Cluster

Use gateways (not cluster routes) to connect data centers. Each region runs its own independent cluster.

```conf
# Region: US-East (nats-us-east-1.conf)
server_name: us-east-1
listen: 0.0.0.0:4222

jetstream {
  server_name: us-east-1
  store_dir: /data/jetstream
  max_mem: 8G
  max_file: 200G
  domain: us-east
}

cluster {
  name: us-east
  listen: 0.0.0.0:6222
  routes: [
    nats-route://us-east-1:6222
    nats-route://us-east-2:6222
    nats-route://us-east-3:6222
  ]
}

gateway {
  name: us-east
  listen: 0.0.0.0:7222

  gateways: [
    { name: us-east,  urls: ["nats://us-east-1:7222", "nats://us-east-2:7222", "nats://us-east-3:7222"] }
    { name: eu-west,  urls: ["nats://eu-west-1:7222", "nats://eu-west-2:7222", "nats://eu-west-3:7222"] }
    { name: ap-south, urls: ["nats://ap-south-1:7222", "nats://ap-south-2:7222", "nats://ap-south-3:7222"] }
  ]
}
```

### Gateway Design Rules
- Each region is an independent NATS cluster with its own JetStream domain
- Gateways handle inter-region message routing automatically
- Use JetStream stream mirroring to replicate data across regions
- Clients connect to their local region — gateways route cross-region traffic
- Gateway connections should use TLS for WAN security

## Leaf Nodes

Leaf nodes extend a cluster to edge locations, remote offices, or isolated environments.

```conf
# Edge location leaf node
server_name: edge-office-1
listen: 0.0.0.0:4222

jetstream {
  store_dir: /data/jetstream
  max_mem: 512M
  max_file: 10G
  domain: edge-office-1
}

leafnodes {
  remotes: [
    {
      urls: ["nats-leaf://main-cluster-1:7422", "nats-leaf://main-cluster-2:7422"]
      account: EDGE
    }
  ]
}
```

Main cluster leaf node listener:

```conf
# Add to main cluster config
leafnodes {
  listen: 0.0.0.0:7422
}
```

### Leaf Node Use Cases
- Edge/IoT deployments with intermittent connectivity
- Development environments connecting to shared staging
- Multi-tenant isolation with account-per-leaf
- Extending JetStream to locations that can't run a full cluster
