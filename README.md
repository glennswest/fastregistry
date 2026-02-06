# FastRegistry

A high-performance, distributed Docker/OCI container registry written in Go. Designed as a fast, scalable alternative to traditional registries with support for horizontal scaling, pull-through caching, and registry synchronization.

## Features

### Core Registry
- **OCI Distribution API v2** - Full compliance with Docker Distribution API v2 specification
- **Docker & OCI Format Support** - Works with Docker manifest v2 and OCI image formats
- **Tag & Digest References** - Access images by tag or content digest

### Performance
- **Memory-Mapped I/O** - Zero-copy reads for blobs under 10MB
- **Content Deduplication** - Identical blobs stored only once
- **LRU Caching** - In-memory cache with TTL support for manifest metadata
- **Concurrent Operations** - Thread-safe with fine-grained locking
- **Streaming Support** - Efficient handling of large blob uploads/downloads

### Storage
- **Content-Addressable Storage** - SHA256 digest-based blob storage
- **BadgerDB Metadata** - ACID-compliant embedded database for durability
- **Atomic Writes** - Digest verification ensures data integrity
- **Range Requests** - Partial blob downloads supported

### Distributed Clustering
- **Mesh Networking** - Gossip-based cluster management via HashiCorp Memberlist
- **Consistent Hashing** - Automatic data distribution across nodes
- **Configurable Replication** - Set replication factor for fault tolerance
- **Auto-Discovery** - Nodes automatically join and leave the cluster
- **Encrypted Communication** - Optional TLS for cluster traffic

### Pull-Through Caching
- **Mirror Upstream Registries** - Cache from Docker Hub, Quay, GCR, and others
- **Upstream Authentication** - Docker Hub token support included
- **Configurable TTL** - Set cache expiration per mirror
- **Transparent Proxy** - Stream and store simultaneously

### Registry Synchronization
- **Quay Sync Support** - Sync entire organizations or specific repositories
- **Tag Filtering** - Include/exclude patterns for selective sync
- **Scheduled Sync** - Cron-style scheduling or continuous mode
- **Signature & SBOM Sync** - Optionally sync container signatures

### Garbage Collection
- **Automatic Cleanup** - Remove unused blobs on schedule
- **Retention Policies** - Keep N most recent tags per repository
- **Dry-Run Mode** - Preview what would be deleted
- **Manual Trigger** - Run GC via admin API

### Security
- **TLS Support** - HTTPS with modern TLS 1.2+ and strong ciphers
- **htpasswd Authentication** - Basic HTTP auth support
- **Open Mode** - Disable auth for internal/trusted networks

### Administration
- **Health Endpoints** - `/health` and `/healthz` for monitoring
- **Status API** - Registry status at `/admin/status`
- **Sync Management** - List, trigger, and monitor sync jobs
- **Metrics** - Prometheus-compatible endpoint

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              HTTP Server                                     │
│                         (TLS, Auth Middleware)                               │
├─────────────────────────────────────────────────────────────────────────────┤
│                              API Router                                      │
├──────────────────┬──────────────────┬──────────────────┬────────────────────┤
│   v2 Handlers    │  Admin Handlers  │  Mirror Proxy    │   Health/Metrics   │
│  (OCI API v2)    │  (GC, Sync, etc) │  (Pull-through)  │                    │
├──────────────────┴──────────────────┴──────────────────┴────────────────────┤
│                           Business Logic                                     │
├──────────────────┬──────────────────┬──────────────────┬────────────────────┤
│   Sync Engine    │  Garbage Coll.   │   Mesh Router    │   Signing (Cosign) │
├──────────────────┴──────────────────┴──────────────────┴────────────────────┤
│                           Storage Layer                                      │
├──────────────────┬──────────────────┬───────────────────────────────────────┤
│   Blob Store     │  Metadata Store  │        LRU Cache                      │
│  (File System)   │   (BadgerDB)     │    (In-Memory)                        │
├──────────────────┴──────────────────┴───────────────────────────────────────┤
│                         Mesh Networking (Optional)                           │
│              (Gossip Protocol, Consistent Hashing, Replication)              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Request Flow

1. **Push Image**: Client → Auth → v2 Handler → Blob Store (chunks) → Metadata Store (manifest/tags)
2. **Pull Image**: Client → Auth → v2 Handler → Cache Check → Blob Store or Mirror Proxy → Stream Response
3. **Mirror Request**: v2 Handler → Mirror Proxy → Upstream Registry → TeeReader (stream + store) → Client

## Code Overview

```
fastregistry/
├── cmd/fastregistry/
│   └── main.go                 # Entry point, server lifecycle, signal handling
│
├── config/
│   └── config.go               # YAML config parsing, validation, defaults
│
├── internal/
│   ├── api/
│   │   ├── router.go           # HTTP router setup, middleware chain
│   │   ├── auth.go             # htpasswd authentication handler
│   │   ├── v2/
│   │   │   └── handlers.go     # OCI Distribution API v2 implementation
│   │   │                       # - Manifest GET/PUT/DELETE
│   │   │                       # - Blob GET/DELETE
│   │   │                       # - Chunked uploads (POST/PATCH/PUT)
│   │   │                       # - Catalog and tag listing
│   │   └── admin/
│   │       └── openshift.go    # Admin endpoints (status, GC trigger, sync)
│   │
│   ├── storage/
│   │   ├── blob.go             # Content-addressable blob storage
│   │   │                       # - Memory-mapped reads for small blobs
│   │   │                       # - Atomic writes with digest verification
│   │   │                       # - Deduplication via content addressing
│   │   ├── metadata.go         # BadgerDB-based metadata store
│   │   │                       # - Manifest and tag storage
│   │   │                       # - Repository tracking
│   │   │                       # - Blob-to-repo relationships
│   │   ├── cache.go            # LRU cache with TTL support
│   │   └── upload.go           # Chunked upload state management
│   │
│   ├── mesh/
│   │   ├── gossip.go           # HashiCorp Memberlist cluster management
│   │   ├── hash.go             # Consistent hashing for data distribution
│   │   ├── replication.go      # Cross-node data replication
│   │   └── router.go           # Request routing in clustered mode
│   │
│   ├── mirror/
│   │   └── proxy.go            # Pull-through cache proxy
│   │                           # - Upstream registry communication
│   │                           # - Docker Hub token auth
│   │                           # - Simultaneous stream and store
│   │
│   ├── sync/
│   │   ├── scheduler.go        # Cron-based job scheduling
│   │   └── quay.go             # Quay-specific sync implementation
│   │                           # - Organization/repo enumeration
│   │                           # - Tag filtering
│   │                           # - Progress tracking
│   │
│   ├── gc/
│   │   └── collector.go        # Garbage collection engine
│   │                           # - Unreferenced blob detection
│   │                           # - Retention policy enforcement
│   │
│   └── signing/
│       └── cosign.go           # Container signature handling
│
├── pkg/
│   ├── digest/                 # SHA256 digest utilities
│   └── oci/                    # OCI/Docker media type constants
│
├── go.mod                      # Go module definition
└── example-config.yaml         # Configuration template
```

### Key Components

| Component | File | Description |
|-----------|------|-------------|
| **Server** | `cmd/fastregistry/main.go` | HTTP server with graceful shutdown, config loading |
| **Router** | `internal/api/router.go` | Chi-based routing with auth middleware |
| **v2 API** | `internal/api/v2/handlers.go` | Full OCI Distribution spec implementation |
| **Blob Store** | `internal/storage/blob.go` | File-based content-addressable storage |
| **Metadata** | `internal/storage/metadata.go` | BadgerDB for manifests, tags, relationships |
| **Cache** | `internal/storage/cache.go` | Generic LRU cache with expiration |
| **Mesh** | `internal/mesh/*.go` | Gossip-based clustering and replication |
| **Mirror** | `internal/mirror/proxy.go` | Transparent upstream registry proxy |
| **Sync** | `internal/sync/*.go` | Scheduled registry synchronization |
| **GC** | `internal/gc/collector.go` | Blob cleanup with retention policies |

## Quick Start

```bash
# Build from source
go build -o fastregistry ./cmd/fastregistry

# Run with defaults (port 5000, storage at /var/lib/fastregistry)
./fastregistry

# Run with custom config
./fastregistry -config=config.yaml

# Override via flags
./fastregistry -storage=/data/registry -addr=:8080
```

## Configuration

Create a `config.yaml`:

```yaml
server:
  addr: ":5000"
  tls:
    cert: /path/to/cert.crt
    key: /path/to/key.key

storage:
  path: /var/lib/fastregistry
  cache_size_mb: 1024
  gc:
    enabled: true
    schedule: "0 2 * * *"
    keep_recent: 10

mesh:
  enabled: false
  bind: ":7946"
  peers: []
  replication_factor: 2

mirrors:
  - name: docker.io
    upstream: https://registry-1.docker.io
    cache_ttl: 24h
  - name: quay.io
    upstream: https://quay.io
    cache_ttl: 24h

sync:
  sources:
    - name: upstream-quay
      type: quay
      url: https://quay.example.com
      token: ${QUAY_TOKEN}
      organizations:
        - myorg
      mode: continuous
      schedule: "*/15 * * * *"
      concurrency: 10

auth:
  type: htpasswd
  htpasswd_file: /etc/fastregistry/htpasswd
```

## API Endpoints

### Registry API (OCI/Docker v2)
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/v2/` | API version check |
| GET | `/v2/_catalog` | List repositories |
| GET/PUT | `/v2/{repo}/manifests/{ref}` | Manifest operations |
| GET | `/v2/{repo}/blobs/{digest}` | Download blob |
| DELETE | `/v2/{repo}/manifests/{ref}` | Delete manifest |
| DELETE | `/v2/{repo}/blobs/{digest}` | Delete blob |
| POST | `/v2/{repo}/blobs/uploads` | Start upload |
| PATCH | `/v2/{repo}/blobs/uploads/{uuid}` | Upload chunk |
| PUT | `/v2/{repo}/blobs/uploads/{uuid}` | Complete upload |
| GET | `/v2/{repo}/tags/list` | List tags |

### Admin API
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/health`, `/healthz` | Health check |
| GET | `/metrics` | Prometheus metrics |
| GET | `/admin/status` | Registry status |
| POST | `/admin/gc` | Trigger garbage collection |
| GET | `/admin/sync/jobs` | List sync jobs |
| POST | `/admin/sync/trigger/{job}` | Trigger sync job |
| GET | `/admin/sync/status/{job}` | Sync job status |

## Use Cases

- **Private Registry** - Host internal container images
- **Pull-Through Cache** - Reduce Docker Hub pull times and rate limits
- **Offline/Air-Gapped** - Container registry for isolated environments
- **CI/CD Pipeline** - Fast local registry for builds
- **Disaster Recovery** - Sync and backup from upstream registries
- **Distributed Deployment** - Multi-node registry with replication

## Requirements

- Go 1.24+ (for building)
- Linux or macOS

## License

MIT
