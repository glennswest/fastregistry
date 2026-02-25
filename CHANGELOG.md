# Changelog

## [Unreleased]

### 2026-02-24
- **feat:** Migrate deployment from mkpod to mkube
- **feat:** Add mkube Pod manifests (fastregistry.yaml, fastregistry-g10.yaml)
- **feat:** Add ConfigMaps for registry configuration
- **refactor:** Replace build-rose.sh with build.sh (registry push workflow)
- **refactor:** Replace deploy-rose.sh/deploy-g10.sh with deploy.sh (mkube registry poll)
- **chore:** Add VERSION file (0.6.0)
- **chore:** Remove mkpod Python deployment scripts

## [v0.6.0] — 2026-02-18

### Added
- FastRegistry-to-FastRegistry replication with full metadata sync
- Sync tab in web UI with status, config, and history views
- Secondary registry deployment (fastregistry.g10.lo)
- Multi-port listening (also_listen config)
- MikroTik RouterOS container deployment (mkpod)
