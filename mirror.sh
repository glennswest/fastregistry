#!/bin/bash
# Mirror an OpenShift release via FastRegistry API
# Triggers clone (download from upstream) + extraction (binaries)
#
# Usage: ./mirror.sh <version> [--arm]
# Example: ./mirror.sh 4.18.10
# Example: ./mirror.sh 4.18.10 --arm

set -e

REGISTRY="http://fastregistry.gw.lo:5000"
ARCH="x86_64"
VERSION=""

for arg in "$@"; do
    case $arg in
        --arm|--aarch64)
            ARCH="aarch64"
            ;;
        -h|--help)
            echo "Usage: $0 <version> [--arm]"
            echo ""
            echo "Mirrors an OpenShift release via the FastRegistry API."
            echo "Automatically discovers, clones, and extracts artifacts."
            echo ""
            echo "Options:"
            echo "  --arm        Mirror aarch64 instead of x86_64"
            echo ""
            echo "Examples:"
            echo "  $0 4.18.10          Mirror x86_64 (default)"
            echo "  $0 4.18.10 --arm    Mirror aarch64"
            exit 0
            ;;
        *)
            VERSION="$arg"
            ;;
    esac
done

if [ -z "$VERSION" ]; then
    echo "Usage: $0 <version> [--arm]"
    echo "Run '$0 --help' for more info"
    exit 1
fi

TAG="${VERSION}-${ARCH}"

echo "=== Mirror OpenShift ${VERSION} (${ARCH}) ==="
echo "Registry: ${REGISTRY}"
echo ""

# Trigger clone (auto-discovers + clones + extracts)
echo "Starting clone..."
RESULT=$(curl -s -X POST "${REGISTRY}/admin/releases/clone" \
    -H 'Content-Type: application/json' \
    -d "{\"version\":\"${TAG}\"}")

# Check for error
if echo "$RESULT" | grep -q '"error"'; then
    echo "Error: $(echo "$RESULT" | jq -r '.error // .message // .')"
    exit 1
fi

echo "Clone started: $(echo "$RESULT" | jq -r '.message // .')"
echo ""

echo "Waiting for completion..."
while true; do
    STATUS=$(curl -s "${REGISTRY}/admin/releases/${TAG}/status")
    STATE=$(echo "$STATUS" | jq -r '.release.state // "unknown"')

    case "$STATE" in
        ready)
            printf "\r%-40s\n" ""
            echo "Release ${TAG} is ready"
            echo ""
            ARTIFACTS=$(echo "$STATUS" | jq -r '.release.artifacts[]?.name // empty' 2>/dev/null)
            if [ -n "$ARTIFACTS" ]; then
                echo "Artifacts:"
                echo "$ARTIFACTS" | while read -r name; do
                    echo "  ${REGISTRY}/files/releases/${VERSION}/${name}"
                done
            fi
            echo ""
            echo "Release image: fastregistry.gw.lo:5000/openshift/release:${TAG}"
            exit 0
            ;;
        failed)
            printf "\r%-40s\n" ""
            ERROR=$(echo "$STATUS" | jq -r '.release.error // "unknown error"')
            echo "Failed: ${ERROR}"
            exit 1
            ;;
        cloning|extracting)
            PROGRESS=$(echo "$STATUS" | jq -r '.progress.percent_done // 0' 2>/dev/null)
            PHASE=$(echo "$STATUS" | jq -r '.progress.phase // .release.state' 2>/dev/null)
            printf "\r  %-20s %s%%" "$PHASE" "$PROGRESS"
            sleep 5
            ;;
        *)
            printf "\r  %-20s" "$STATE"
            sleep 5
            ;;
    esac
done
