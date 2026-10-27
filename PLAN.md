# CNCM plan: feature parity with `care_clinic`

Goal: everything a clinic can do in [`ohcnetwork/care_clinic`](https://github.com/ohcnetwork/care_clinic)
is doable from the CNCM UI by someone with basic computer skills. The two-step
replacement is:

1. **Containers Anywhere (CA)** owns the machine: VM, K3s, LAN/TLS/mDNS gateway,
   host folders, host-level recovery points, autostart.
2. **CNCM** owns CARE: build, deploy, configure, back up, update, operate.

Anything that needs host access is requested from CA through the contract in
[CONTAINERS_ANYWHERE_API.md](CONTAINERS_ANYWHERE_API.md); CNCM never reaches for the
host directly. Status below is honest: nothing here has run on a live appliance.

## Done in this change: Containers Anywhere 0.2.0 compatibility

0.2.0 (`app-v0.2.0` / `appliance-v0.2.0`) no longer preinstalls CNCM and only routes
traffic that has a `WorkloadPublication`. CNCM previously created a plain Ingress, which
would have been unreachable. Now:

- CARE install creates the Ingress plus `care` (LAN, mDNS, local-CA HTTPS) and
  `care-localhost` (loopback) publications. `http://` addresses request plain HTTP.
  Hostnames must sit under `care.local`/`care.localhost` (the 0.2.0 admission rule), so
  other addresses are left unpublished. A cluster without the CRD skips publication
  with a stage message instead of failing the install.
- Derived CARE address is `https://care.care.local[:port]`; session cookies are `Secure`
  whenever the browser origin is HTTPS in auto mode.
- Kite install is one paste: `manifests/cncm-containers-anywhere.yaml` (manager +
  publication, `CNCM_ORIGIN=auto`), kept in sync by a test and `make kite-manifest`.
- No token authoring: CNCM creates Secret `cncm-bootstrap` on first start; the operator
  copies it from Kite. A pre-created Secret still wins.

Still unverified: the publication admission rules on a real 0.2.0 appliance, and the
HTTP-registry/containerd problem below.

## Parity matrix

| `care_clinic` capability | Owner | Status in CNCM |
| --- | --- | --- |
| Install CARE (backend, frontend, DB, files, Redis) | CNCM | Done (POC, unverified live) |
| Start / stop / restart | CNCM | Done |
| Pod status, events, redacted logs | CNCM | Done |
| Clinic `.local` address, LAN access, HTTPS | CA + CNCM | Publication done; trust onboarding open |
| Phone/tablet certificate QR page | CA (`/root.crt`, status) + CNCM UI | Not started |
| Setup wizard with checks (disk, RAM, address clash) | CNCM + CA status | Preflight only |
| Encrypted nightly backups, retention, backup-now | CNCM (CNPG + Silo) + CA destination | Not started |
| Backup recovery key + verification + password recovery codes | CNCM | Not started |
| Restore from file into staging, cutover, journal | CNCM | Not started |
| Backup space check, stale/failed alerts | CNCM + CA free-space status | Not started |
| Settings editor (everyday + support-only keys) | CNCM | Not started |
| Reset a CARE web password | CNCM | Not started |
| CNCM admin password change / recovery codes | CNCM | Not started |
| Plugins catalog, apply, rollback | CNCM | Not started |
| CARE updates (check, stage, install) | CNCM | Blocked on verified restore |
| Manager app updates | CNCM image + Kite | Documented only |
| Storage page and safe cleanup (old builds/images) | CNCM + CA disk status | Not started |
| Web firewall (Coraza mode) | CNCM (proxy image) | Not started |
| Start at login, keep awake, host reboot recovery | CA | CA work |
| Host-wide recovery point, off-device copy, new-machine restore | CA | CA work |
| Home access via Tailscale | CNCM workload + CA egress | Later |
| Uninstall with explicit data choices | CNCM | Not started |
| CARE Onboarding plugin | CNCM plugin catalog | After plugins |
| Migration from `care_clinic` | none | Out of scope |

## Next phases (CNCM only, in this order)

Each phase ends with local tests; live acceptance waits for an authorized disposable
cluster.

1. **Operate and configure** (no recovery dependency)
   - Settings editor over a declared allow-list of CARE env keys, stored in SQLite,
     rendered into ConfigMap/Secret, applied by rolling the CARE deployments.
   - Reset CARE web password via a one-off Job (`manage.py`) with credentials passed
     as Secret env, never arguments.
   - CNCM password change and offline recovery codes (hashes only).
   - Plugin catalog (reviewed list), pending/apply with automatic rollback to the
     previous release.
2. **Backups and restore** (the gate for everything destructive)
   - Destination: S3-compatible endpoint first (CA 0.2.x destination or external).
   - CNPG `ObjectStore` + `ScheduledBackup` with WAL archiving, retention, status page
     showing earliest/latest recoverable time and archive lag, not just job success.
   - Silo objects via a scheduled Job to the same destination; encrypt client-side.
   - Recovery key creation, verified export, re-download behind password.
   - Restore to a new database and bucket, validate, explicit cutover, durable journal;
     restore drill that proves a data marker.
   - Pre-write free-space checks and failed/stale banners.
3. **Updates**: only after phase 2 restore is proven. Pre-update recovery point,
   migration on staged database, rollback by image digest, update-check toggle.
4. **Storage, firewall, trust onboarding**: build and image cleanup with preview,
   Coraza mode, certificate QR/instructions page driven by CA status.
5. **Lifecycle**: guided uninstall with explicit choices; Tailscale workload recipe.

## Containers Anywhere 0.3.0 status (checked against `develop`, docs/HOST_API.md)

Shipped and now used by CNCM when present: `HostStatus` (#11), `HostRequest` for resize,
autostart, power, prune and LAN check (#12, #17 partly, #18 partly), `HostRegistryTrust`
(#10), root-CA/ports status (#13), update state read-only (#19).
Still open in Containers Anywhere, deliberately left there: destinations with an S3
endpoint (#14), recovery points/schedules (#15), quiesce hooks, kit, restore drill (#16),
keep-awake and suspend handling (#17), the umbrella (#3) and unverified items (#20).
Backups, PITR and anything needing those stay in the paused phases above.

## Remaining decisions

- **Sizing**: CNCM now warns below 8 GiB and can request a resize; a human must approve.
- **First boot needs internet** in 0.2.0/0.3.0, so no fully offline clinic yet.
- **Backup-destination ownership**: assumes CA's S3-compatible endpoint (#14).

## Rules kept

Single operator, serialized mutations, persisted stages, no silent migration replay,
no secrets in logs, README claims limited to what is verified, no deploy to any cluster
without explicit authorization of that target.
