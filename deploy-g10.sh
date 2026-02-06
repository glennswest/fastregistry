#!/bin/bash
set -e

TAR_NAME="fastregistry.tar"
ROSE_HOST="admin@rose1.gw.lo"
ROSE_TARBALL_PATH="raid1/tarballs"

echo "=== Deploying FastRegistry to g10 ==="

# Check if tarball exists
if [ ! -f "${TAR_NAME}" ]; then
    echo "Error: ${TAR_NAME} not found. Run ./build-rose.sh first."
    exit 1
fi

# Upload tarball to rose1
echo "Uploading ${TAR_NAME} to rose1..."
rsync -av --progress ${TAR_NAME} ${ROSE_HOST}:${ROSE_TARBALL_PATH}/

# Run mkpod deployment
echo "Running mkpod deployment..."
cd ../mkpod
source venv/bin/activate
python3 ../fastregistry/deploy_g10.py "$@"

echo ""
echo "Deployment complete!"
echo "FastRegistry g10 running at http://192.168.10.202:5000 and http://192.168.10.202:80"
