# Changelog

## [Unreleased]

### 2026-04-30
- **fix:** Declare missing `data` and `secrets` PVCs in fastregistry.yaml so the mkpod→mkube pod actually has volume bindings (mkube resolves PVC `<ns>/<name>` to `/raid1/volumes/pvc/<ns>_<name>/`)
- **chore:** Wiped legacy data and brought the pod up clean: pull-secret.json placed at `/raid1/volumes/pvc/g10_fastregistry-secrets/pull-secret.json`, fastregistry serving at `192.168.10.50:5000` (HTTP 200, empty catalog ready for re-mirror)
- **chore:** Updated `fastregistry.gw.lo` A record (gw.lo zone) to `192.168.10.50` so install-config consumers reach the new pod

### 2026-05-01
- **chore:** Moved hostname from `fastregistry.gw.lo` to `fastregistry.g8.lo` (deleted record from gw.lo zone, created A record in g8.lo zone → `192.168.10.50`)
- **docs:** Update RELEASES.md examples to use `fastregistry.g8.lo:5000`
- **fix:** Bundle CA certificates into the rose container (`Dockerfile.rose`) so outbound HTTPS to quay.io / registry.redhat.io / ghcr.io for release discovery and clone works (previous scratch image had no `/etc/ssl/certs/ca-certificates.crt`, causing `x509: certificate signed by unknown authority` on every upstream call)

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
