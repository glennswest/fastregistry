#!/usr/bin/env python3
"""Deploy FastRegistry to rose1.gw.lo as fastregistry.g10.lo (MikroTik RouterOS container)"""

import sys

# Add mkpod to path
sys.path.insert(0, '/Volumes/minihome/gwest/projects/mkpod')
import mkpod

mbase = "raid1/volumes"

# Container settings
CONTAINER_NAME = "fastregistry.g10.lo"
IP_ADDRESS = "192.168.10.202"

# Delete existing pod if redeploying (pass --redeploy flag)
if "--redeploy" in sys.argv:
    print(f"Removing existing container {CONTAINER_NAME}...")
    mkpod.delete_pod(CONTAINER_NAME)

# Set up mounts
print("Setting up mounts...")

# Config mount (/etc/fastregistry) - for config.yaml and pull-secret.json
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
print(f"Deploying {CONTAINER_NAME} with IP {IP_ADDRESS}...")
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
print(f"{CONTAINER_NAME} running at http://{IP_ADDRESS}:5000")
