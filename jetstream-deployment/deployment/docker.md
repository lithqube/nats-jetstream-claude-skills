# Docker Deployment

## Single-Node Development

```yaml
# docker-compose.yml
services:
  nats:
    image: nats:2.10.24
    command: ["-js", "-m", "8222"]
    ports:
      - "4222:4222"   # client
      - "8222:8222"   # monitoring
    volumes:
      - nats-data:/data/jetstream

volumes:
  nats-data:
```

```bash
docker compose up -d
```

## Single-Node with Custom Config

```yaml
# docker-compose.yml
services:
  nats:
    image: nats:2.10.24
    command: ["-c", "/etc/nats/nats-server.conf"]
    ports:
      - "4222:4222"
      - "8222:8222"
    volumes:
      - ./nats-server.conf:/etc/nats/nats-server.conf:ro
      - nats-data:/data/jetstream

volumes:
  nats-data:
```

```conf
# nats-server.conf
listen: 0.0.0.0:4222
http: 0.0.0.0:8222

jetstream {
  store_dir: /data/jetstream
  max_mem: 1G
  max_file: 10G
}

max_payload: 8MB
```

## 3-Node Cluster for Local Development

```yaml
# docker-compose-cluster.yml
services:
  nats-1:
    image: nats:2.10.24
    command: ["-c", "/etc/nats/nats.conf", "--name", "nats-1"]
    ports:
      - "4222:4222"
      - "8222:8222"
    volumes:
      - ./nats-cluster.conf:/etc/nats/nats.conf:ro
      - nats-1-data:/data/jetstream
    networks:
      - nats-net

  nats-2:
    image: nats:2.10.24
    command: ["-c", "/etc/nats/nats.conf", "--name", "nats-2"]
    ports:
      - "4223:4222"
      - "8223:8222"
    volumes:
      - ./nats-cluster.conf:/etc/nats/nats.conf:ro
      - nats-2-data:/data/jetstream
    networks:
      - nats-net

  nats-3:
    image: nats:2.10.24
    command: ["-c", "/etc/nats/nats.conf", "--name", "nats-3"]
    ports:
      - "4224:4222"
      - "8224:8222"
    volumes:
      - ./nats-cluster.conf:/etc/nats/nats.conf:ro
      - nats-3-data:/data/jetstream
    networks:
      - nats-net

volumes:
  nats-1-data:
  nats-2-data:
  nats-3-data:

networks:
  nats-net:
    driver: bridge
```

```conf
# nats-cluster.conf
listen: 0.0.0.0:4222
http: 0.0.0.0:8222

jetstream {
  store_dir: /data/jetstream
  max_mem: 1G
  max_file: 10G
}

cluster {
  name: local-cluster
  listen: 0.0.0.0:6222
  routes: [
    nats-route://nats-1:6222
    nats-route://nats-2:6222
    nats-route://nats-3:6222
  ]
  connect_retries: 30
}
```

```bash
docker compose -f docker-compose-cluster.yml up -d

# Verify cluster
docker exec -it nats-1 nats-server --help  # check version
curl http://localhost:8222/jsz               # JetStream status
curl http://localhost:8222/routez            # cluster routes
```

## Docker Run (Quick Start)

```bash
# Minimal JetStream server
docker run -d --name nats \
  -p 4222:4222 \
  -p 8222:8222 \
  -v nats-data:/data/jetstream \
  nats:2.10.24 \
  -js -m 8222 -sd /data/jetstream

# Verify
docker exec nats nats-server --version
curl http://localhost:8222/healthz
```

## Environment Variables

NATS server doesn't use environment variables directly, but you can use them in Docker Compose with config templating:

```yaml
services:
  nats:
    image: nats:2.10.24
    command:
      - "-js"
      - "-m"
      - "8222"
      - "--max_payload"
      - "${NATS_MAX_PAYLOAD:-8MB}"
      - "--max_connections"
      - "${NATS_MAX_CONNECTIONS:-64000}"
    ports:
      - "${NATS_CLIENT_PORT:-4222}:4222"
      - "${NATS_MONITOR_PORT:-8222}:8222"
```

## Health Check in Docker Compose

```yaml
services:
  nats:
    image: nats:2.10.24
    command: ["-js", "-m", "8222"]
    healthcheck:
      test: ["CMD", "wget", "--spider", "-q", "http://localhost:8222/healthz"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 10s
```
