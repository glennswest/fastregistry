# Changelog

## [Unreleased]

### 2026-04-30
- **fix:** Declare missing `data` and `secrets` PVCs in fastregistry.yaml; the mkpod→mkube migration left the pod with no volume bindings, so the OCP mirror cache was never mounted (host data preserved at `/raid1/registry/{blobs,repositories,metadata,uploads}` from the previous deployment must be moved to `/raid1/volumes/pvc/g10_fastregistry-data/` before re-apply)

### 2026-02-24
- **feat:** Migrate deployment from mkpod to mkube
- **feat:** Add mkube Pod manifest with ConfigMap (fastregistry.yaml)
- **refactor:** Replace build-rose.sh with build.sh (registry push workflow)
- **refactor:** Replace deploy scripts with deploy.sh (build + push)
- **chore:** Add VERSION file (0.6.0)
- **chore:** Remove mkpod Python deployment scripts
- **chore:** Remove g10 replica (single instance only)

## [v0.6.0] — 2026-02-18

### Added
- FastRegistry-to-FastRegistry replication with full metadata sync
- Sync tab in web UI with status, config, and history views
- Secondary registry deployment (fastregistry.g10.lo)
- Multi-port listening (also_listen config)
- MikroTik RouterOS container deployment (mkpod)
