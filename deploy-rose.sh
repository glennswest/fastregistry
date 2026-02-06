#!/bin/bash
# Deploy FastRegistry to rose.gw.lo as ARM container
set -e

HOST="rose.gw.lo"
USER="root"
REMOTE_DIR="/opt/fastregistry"
IMAGE="fastregistry:latest"

echo "=== FastRegistry ARM Container Deployment ==="

# Build ARM64 binary locally
echo "Building ARM64 binary..."
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o fastregistry-arm64 ./cmd/fastregistry

# Create deployment package
echo "Creating deployment package..."
TMPDIR=$(mktemp -d)
cp fastregistry-arm64 "$TMPDIR/fastregistry"
cp docker-compose.yml "$TMPDIR/"

# Create Dockerfile for ARM (simpler, just copies binary)
cat > "$TMPDIR/Dockerfile" << 'EOF'
FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY fastregistry /usr/local/bin/fastregistry
EXPOSE 5000
ENTRYPOINT ["/usr/local/bin/fastregistry"]
CMD ["-config", "/etc/fastregistry/config.yaml"]
EOF

# Create example config
cat > "$TMPDIR/config.yaml" << 'EOF'
server:
  addr: ":5000"

storage:
  path: /data/registry
  cache_size_mb: 2048

releases:
  enabled: true
  pull_secret: /etc/fastregistry/pull-secret.json
  upstream: quay.io
  repository: openshift-release-dev/ocp-release
  local_repo: openshift/release
  architectures:
    - x86_64
    - aarch64
  auto_discover: true
EOF

# Copy to remote
echo "Copying files to ${HOST}..."
ssh ${USER}@${HOST} "mkdir -p ${REMOTE_DIR}"
scp -r "$TMPDIR"/* ${USER}@${HOST}:${REMOTE_DIR}/

# Build and start on remote
echo "Building container on ${HOST}..."
ssh ${USER}@${HOST} "cd ${REMOTE_DIR} && docker build -t ${IMAGE} ."

echo "Starting container..."
ssh ${USER}@${HOST} "cd ${REMOTE_DIR} && docker compose down 2>/dev/null || true && docker compose up -d"

# Cleanup
rm -rf "$TMPDIR"
rm -f fastregistry-arm64

echo ""
echo "=== Deployment complete ==="
echo "FastRegistry running at http://${HOST}:5000"
echo ""
echo "To check status: ssh ${USER}@${HOST} 'docker logs fastregistry'"
echo "To copy pull-secret: scp pull-secret.json ${USER}@${HOST}:${REMOTE_DIR}/"
