#!/usr/bin/env python3
"""Deploy FastRegistry to rose1.gw.lo (MikroTik RouterOS container)"""

import sys
import os

# Add mkpod to path
sys.path.insert(0, '/Volumes/minihome/gwest/projects/mkpod')
import mkpod

mbase = "raid1/volumes"

# Container settings
CONTAINER_NAME = "fastregistry.gw.lo"
IP_ADDRESS = "192.168.10.50"

# Delete existing pod if redeploying (pass --redeploy flag)
if "--redeploy" in sys.argv:
    print("Removing existing container...")
    mkpod.delete_pod(CONTAINER_NAME)

# Set up mounts
# Config mount (/etc/fastregistry) - for config.yaml and pull-secret.json
print("Setting up mounts...")
mkpod.add_mount(
    f"{CONTAINER_NAME}.config",
    f"{mbase}/{CONTAINER_NAME}.config",
    "/etc/fastregistry"
)

# Data mount (/data/registry) - for registry storage
mkpod.add_mount(
    f"{CONTAINER_NAME}.data",
    f"{mbase}/{CONTAINER_NAME}.data",
    "/data/registry"
)

# Deploy container
print(f"Deploying container with IP {IP_ADDRESS}...")
mkpod.direct_pod(
    "fastregistry.tar",
    CONTAINER_NAME,
    CONTAINER_NAME,
    [f"{CONTAINER_NAME}.config", f"{CONTAINER_NAME}.data"],
    ip_address=IP_ADDRESS
)

print("")
print("=" * 50)
print("Deployment complete!")
print(f"FastRegistry running at http://{IP_ADDRESS}:5000")
print("")
print("First time setup:")
print(f"  1. Create config directory on rose1:")
print(f"     ssh {mkpod.default_host} 'mkdir -p {mbase}/{CONTAINER_NAME}.config'")
print("")
print(f"  2. Upload config.yaml:")
print(f"     scp config.yaml admin@{mkpod.default_host}:{mbase}/{CONTAINER_NAME}.config/")
print("")
print(f"  3. Upload pull-secret.json:")
print(f"     scp pull-secret.json admin@{mkpod.default_host}:{mbase}/{CONTAINER_NAME}.config/")
print("")
print("View logs:")
print(f"  ssh admin@{mkpod.default_host} '/log/print where topics~\"container\"'")
