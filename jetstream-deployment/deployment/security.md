# Security

## TLS Configuration

### Server TLS (clients connect with TLS)

```conf
# nats-server.conf
tls {
  cert_file: /etc/nats/certs/server-cert.pem
  key_file: /etc/nats/certs/server-key.pem
  ca_file: /etc/nats/certs/ca.pem
  timeout: 5
}
```

Client connection with TLS:

```go
nc, _ := nats.Connect("nats://localhost:4222",
    nats.RootCAs("/path/to/ca.pem"),
)
```

### Mutual TLS (mTLS)

Both server and client authenticate with certificates:

```conf
tls {
  cert_file: /etc/nats/certs/server-cert.pem
  key_file: /etc/nats/certs/server-key.pem
  ca_file: /etc/nats/certs/ca.pem
  verify: true             # require client certificates
  timeout: 5
}
```

Client connection with mTLS:

```go
nc, _ := nats.Connect("nats://localhost:4222",
    nats.RootCAs("/path/to/ca.pem"),
    nats.ClientCert("/path/to/client-cert.pem", "/path/to/client-key.pem"),
)
```

### Cluster TLS

```conf
cluster {
  name: nats-cluster
  listen: 0.0.0.0:6222

  tls {
    cert_file: /etc/nats/certs/cluster-cert.pem
    key_file: /etc/nats/certs/cluster-key.pem
    ca_file: /etc/nats/certs/ca.pem
    verify: true
    timeout: 5
  }

  routes: [
    nats-route://nats-1:6222
    nats-route://nats-2:6222
    nats-route://nats-3:6222
  ]
}
```

### Certificate Generation (for development)

```bash
# Using mkcert for local development
mkcert -install
mkcert -cert-file server-cert.pem -key-file server-key.pem \
  localhost 127.0.0.1 ::1 nats-1 nats-2 nats-3

# Using openssl for production
# Generate CA
openssl genrsa -out ca-key.pem 4096
openssl req -new -x509 -key ca-key.pem -out ca.pem -days 365 \
  -subj "/CN=NATS CA"

# Generate server cert
openssl genrsa -out server-key.pem 2048
openssl req -new -key server-key.pem -out server.csr \
  -subj "/CN=nats-server"
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out server-cert.pem -days 365 \
  -extfile <(echo "subjectAltName=DNS:nats-1,DNS:nats-2,DNS:nats-3,DNS:localhost")
```

## Authentication

### Token-Based (simple, development only)

```conf
authorization {
  token: "s3cret-t0ken"
}
```

```go
nc, _ := nats.Connect("nats://localhost:4222", nats.Token("s3cret-t0ken"))
```

### User/Password

```conf
authorization {
  users: [
    { user: "admin", password: "$2a$11$..." }  # bcrypt hash
    { user: "app-orders", password: "$2a$11$...",
      permissions: {
        publish: { allow: ["orders.>"] }
        subscribe: { allow: ["orders.>", "_INBOX.>"] }
      }
    }
    { user: "app-analytics", password: "$2a$11$...",
      permissions: {
        publish: { deny: [">"] }
        subscribe: { allow: ["orders.>", "payments.>"] }
      }
    }
  ]
}
```

### NKey Authentication (recommended for production)

NKeys are Ed25519 key pairs. The server only stores the public key.

```bash
# Generate NKey pair using nk tool
nk -gen user -pubout
# Output:
# SUAM... (seed/private key — keep secret)
# UA...   (public key — put in server config)
```

```conf
authorization {
  users: [
    { nkey: "UA7BQBSMZ..." }
    { nkey: "UB3DKQMSL...",
      permissions: {
        publish: { allow: ["orders.>"] }
        subscribe: { allow: ["orders.>", "_INBOX.>"] }
      }
    }
  ]
}
```

```go
opt, _ := nats.NkeyOptionFromSeed("/path/to/user.nk")
nc, _ := nats.Connect("nats://localhost:4222", opt)
```

### JWT / Decentralized Authentication (recommended for multi-tenant)

Uses the `nsc` tool to manage operators, accounts, and users. No server restart needed when adding users.

```bash
# Install nsc
brew install nats-io/nats-tools/nsc  # macOS
# or: go install github.com/nats-io/nsc/v2@latest

# Create operator
nsc add operator MyOrg

# Create account
nsc add account OrderService

# Create user with permissions
nsc add user --account OrderService order-publisher \
  --allow-pub "orders.>" \
  --allow-sub "_INBOX.>"

# Export credentials file
nsc generate creds --account OrderService --name order-publisher > order-publisher.creds
```

Server configuration for JWT auth:

```conf
operator: /etc/nats/operator.jwt
system_account: SYS

resolver: {
  type: full
  dir: /etc/nats/jwt
  allow_delete: false
  interval: "2m"
}
```

Client connection with credentials:

```go
nc, _ := nats.Connect("nats://localhost:4222",
    nats.UserCredentials("/path/to/order-publisher.creds"),
)
```

## Authorization

### Subject-Level Permissions

```conf
authorization {
  users: [
    {
      user: "order-service"
      password: "..."
      permissions: {
        publish: {
          allow: ["orders.>", "$JS.API.>"]   # allow JetStream API
          deny: ["admin.>"]
        }
        subscribe: {
          allow: ["orders.>", "_INBOX.>"]
        }
        allow_responses: { max: 1, expires: "5s" }  # allow request-reply
      }
    }
  ]
}
```

### Account-Based Isolation (multi-tenant)

```conf
accounts {
  ORDERS {
    users: [{ user: "order-app", password: "..." }]
    jetstream: {
      max_mem: 1G
      max_file: 20G
      max_streams: 10
      max_consumers: 50
    }
  }

  ANALYTICS {
    users: [{ user: "analytics-app", password: "..." }]
    jetstream: {
      max_mem: 2G
      max_file: 50G
      max_streams: 5
      max_consumers: 20
    }
  }

  SYS {
    users: [{ user: "admin", password: "..." }]
  }
}

system_account: SYS
```

### Export/Import Between Accounts

Allow the analytics account to subscribe to order events:

```conf
accounts {
  ORDERS {
    users: [{ user: "order-app", password: "..." }]
    exports: [
      { stream: "orders.>" }  # export as stream (pub/sub)
    ]
  }

  ANALYTICS {
    users: [{ user: "analytics-app", password: "..." }]
    imports: [
      { stream: { account: ORDERS, subject: "orders.>" } }
    ]
  }
}
```
