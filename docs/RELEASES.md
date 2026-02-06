# FastRegistry Release Management

FastRegistry provides complete OpenShift release management including discovery, cloning, artifact extraction, and agent ISO generation - all without requiring `openshift-install` on the client.

## Features

- **Release Discovery**: Automatically discover releases from upstream (quay.io)
- **Release Cloning**: Mirror release images to local registry
- **Artifact Extraction**: Extract binaries (openshift-install, oc) and CoreOS ISO
- **ISO Generation**: Generate bootable agent ISOs with embedded ignition
- **Download Tracking**: Track artifact downloads with usage statistics
- **Event Timeline**: View history of discovery, cloning, and download events

## Quick Start

### 1. Mirror a Release

```bash
# Clone a release (discovers, downloads, and extracts artifacts)
curl -X POST "http://fastregistry.gw.lo:5000/admin/releases/clone" \
  -H "Content-Type: application/json" \
  -d '{"version": "4.21.0-x86_64"}'

# Check status
curl -s "http://fastregistry.gw.lo:5000/admin/releases/4.21.0-x86_64/status" | jq
```

Or use the mirror script:
```bash
./mirror.sh 4.21.0           # x86_64 (default)
./mirror.sh 4.21.0 --arm     # aarch64
```

### 2. Download Artifacts

Once a release is ready, artifacts are available at:
```
http://fastregistry.gw.lo:5000/files/releases/<version>/<artifact>
```

Available artifacts:
- `openshift-install` - Linux installer binary
- `openshift-install-mac` - macOS Intel installer
- `openshift-install-mac-arm64` - macOS ARM installer
- `oc` - Linux oc client
- `oc-mac` - macOS Intel oc client
- `oc-mac-arm64` - macOS ARM oc client
- `oc.exe` - Windows oc client
- `coreos.iso` - Base CoreOS live ISO

### 3. Generate Agent ISO (No openshift-install Required)

Create a bootable agent ISO by posting your install configs:

```bash
curl -X POST "http://fastregistry.gw.lo:5000/admin/releases/4.21.0-x86_64/iso" \
  -H "Content-Type: application/json" \
  -d "{
    \"install_config\": $(cat install-config.yaml | jq -Rs),
    \"agent_config\": $(cat agent-config.yaml | jq -Rs)
  }"
```

Response:
```json
{
  "id": "48548d80-10f7-424f-aab7-1903b22c494d",
  "iso_url": "/files/installs/48548d80-10f7-424f-aab7-1903b22c494d/agent.iso",
  "full_url": "http://fastregistry.gw.lo:5000/files/installs/48548d80-10f7-424f-aab7-1903b22c494d/agent.iso"
}
```

The `full_url` can be used directly for PXE sanboot.

### 4. Copy Artifacts to Remote Server

```bash
curl -X POST "http://fastregistry.gw.lo:5000/admin/releases/4.21.0-x86_64/copy" \
  -H "Content-Type: application/json" \
  -d '{"artifact": "coreos.iso", "destination": "root@pxe.gw.lo:/srv/tftp/iso/"}'
```

## API Reference

### Discovery & Cloning

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/admin/releases` | GET | List all releases |
| `/admin/releases?state=ready` | GET | List releases by state |
| `/admin/releases/discover` | POST | Trigger upstream discovery |
| `/admin/releases/clone` | POST | Clone a release `{"version": "4.21.0-x86_64"}` |
| `/admin/releases/{version}/status` | GET | Get release status and progress |
| `/admin/releases/{version}/artifacts` | GET | List extracted artifacts |

### Extraction & Management

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/admin/releases/extract` | POST | Extract artifacts `{"version": "..."}` or `{"all": true}` |
| `/admin/releases/{version}/extract` | POST/GET | Extract artifacts for specific version |
| `/admin/releases/{version}/reset` | POST | Reset stuck release to cloned state |

### ISO Generation

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/admin/releases/{version}/iso` | POST | Generate agent ISO (see below) |
| `/admin/releases/{version}/copy` | POST | Copy artifact to remote server |

**ISO Generation Request Body:**
```json
{
  "install_config": "<install-config.yaml content>",
  "agent_config": "<agent-config.yaml content>"
}
```

Or with pre-generated ignition:
```json
{
  "ignition": "<ignition JSON>"
}
```

### File Downloads

| Endpoint | Description |
|----------|-------------|
| `/files/releases/{version}/{artifact}` | Download extracted artifact |
| `/files/installs/{uuid}/agent.iso` | Download generated agent ISO |

## Configuration

Add to `fastregistry.yaml`:

```yaml
releases:
  enabled: true
  pull_secret: /path/to/pull-secret.json
  upstream: quay.io
  repository: openshift-release-dev/ocp-release
  local_repo: openshift/release
  architectures:
    - x86_64
    - aarch64
  auto_discover: true
```

## Server Requirements

For ISO generation, the server needs:
- `coreos-installer` package installed (`dnf install coreos-installer`)

## Web UI

Access the releases dashboard at:
```
http://fastregistry.gw.lo:5000/ui/releases
```

Features:
- **Browse**: View releases grouped by major.minor version
- **News**: See newly discovered releases
- **Cloning**: Live progress of active clones
- **Events**: Timeline of all release events
- **Usage**: Download statistics and history
- **Log**: Discovery log output

## Example: Complete Agent-Based Install Workflow

```bash
# 1. Prepare your configs
cat > install-config.yaml << 'EOF'
apiVersion: v1
baseDomain: example.com
metadata:
  name: my-cluster
# ... rest of config
EOF

cat > agent-config.yaml << 'EOF'
apiVersion: v1alpha1
metadata:
  name: my-cluster
rendezvousIP: 192.168.1.100
# ... rest of config
EOF

# 2. Generate agent ISO
ISO_URL=$(curl -s -X POST "http://fastregistry.gw.lo:5000/admin/releases/4.21.0-x86_64/iso" \
  -H "Content-Type: application/json" \
  -d "{
    \"install_config\": $(cat install-config.yaml | jq -Rs),
    \"agent_config\": $(cat agent-config.yaml | jq -Rs)
  }" | jq -r '.full_url')

echo "ISO ready at: $ISO_URL"

# 3. Boot nodes from the ISO URL via PXE sanboot
# The ISO contains everything needed for agent-based installation
```

## Changelog (Recent)

- **d6a69ac** - Add server-side ignition generation from YAML configs
- **169f610** - Add ISO generation API for agent-based installs
- **06f661a** - Add artifact copy API for SCP to remote servers
- **5063a45** - Add deploy.sh script for production deployment
- **8921886** - Add CoreOS ISO extraction from machine-os-images component
- **fb1934a** - Add Mac/Windows binaries extraction and state reset endpoint
- **1f64e14** - Add cloning status, events timeline, and usage tracking tabs
- **f3cd04e** - Fix extractor to pull component images and add mirror API
