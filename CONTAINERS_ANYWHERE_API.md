# Requests to Containers Anywhere (for CNCM-managed host operations)

> **Status update (0.3.0):** P0 items 1–4 and the status/sizing/power/autostart/prune/update-state
> parts of P2 shipped as `HostStatus`, `HostRequest` and `HostRegistryTrust` (see Containers Anywhere
> `docs/HOST_API.md`; the registry mechanism chose the generic admin-approved trust, not a fixed
> address). Open there: #14 destinations, #15 recovery points, #16 quiesce/kit/restore, #17 keep-awake.
> The text below is the original proposal; the live issues and HOST_API.md are authoritative.

Audience: `tellmeY18/containers_anywhere`, after 0.2.0 stabilises. This is a proposal,
not a contract. It builds on what 0.2.0 already has: the `WorkloadPublication` CRD,
RBAC through `containers-workload-publisher`, and the exploratory `RECOVERY_OPTIONS.md`
(`/_agent/backup/*`, Level 1 appliance recovery points vs Level 2 workload PITR).

## Shape: Kubernetes-native, not a new transport

0.2.0 already proved the pattern: a CRD that a workload service account may write under
an administrator-granted RBAC role, reconciled by a host-side controller over the
private vsock/mTLS channel, with `status` written back. Reuse it. Then Kite, `kubectl`
and CNCM all use one API; no host capability token is ever given to a pod; and the
launcher UI remains the only place that selects host folders or approves destructive
steps. Suggested group: `host.containersanywhere.dev/v1alpha1`.

Rules that apply to every resource below:

- Workloads can **request and read**; only the launcher UI can enrol destinations,
  export keys, approve cutover or delete recovery points.
- Status reports recoverability (last completed, last verified, earliest and latest
  recoverable, age, degraded reason), never only "job ran".
- Missing host feedback becomes `Unknown` after a timeout, as in `WorkloadPublication`.
- Keys never enter the cluster. Writers cannot delete or shorten retention.

## What CNCM needs, by priority

### P0: unblockers for the CARE install

| # | Request | Why |
| --- | --- | --- |
| 1 | **Registry trust.** Appliance configures containerd to pull from the in-cluster Distribution registry over HTTP (fixed `localhost:30500` mirror in `registries.yaml`, or a `HostRegistryTrust` resource limited to that address). | CNCM builds with BuildKit and pulls by digest; it will not edit node config. |
| 2 | **`HostStatus`** (singleton, read-only). Appliance and app version, `bundleID`, K3s version, CPUs and RAM, data-disk size, used, free and pressure flag, host free space under the state dir, uptime, clock skew, autostart state, update-available. | Setup preflight, Storage page, capacity warnings, honest backups banners. |
| 3 | **Sizing.** `HostResources` status plus a request that applies on next restart with launcher approval. | CARE needs about 8 GiB; defaults are 4 GiB. |
| 4 | **Certificate and reachability status.** Public root CA URL and fingerprint, per-publication `CertificateReady`, and an optional LAN self-check result (firewall, multicast). 0.2.0 already has the first two on the publication. | Phone/tablet onboarding page, replacing `care_clinic`'s `/setup` flow. |

### P1: host-level backup, snapshot, PITR enablement

1. **`HostDestination`** (cluster-scoped; created only from the launcher UI).
   Types: local folder, removable disk, later Drive/consumer cloud. Status: available,
   free bytes, missing-device, `sameDeviceAsData` (label honestly: not disaster
   recovery), immutability, last checked. **Exposes an S3-compatible endpoint** (a
   ClusterIP Service backed by the host folder) with two credentials in separate
   Secrets: a *writer* (put, no delete) and a *retention* credential held by the host
   only. This lets CNPG Barman Cloud and CNCM file backups write there with no CNCM
   code for host storage, and keeps PITR at Level 2 where it belongs.
2. **`HostRecoveryPoint`** (request plus status). Spec: destination, requested
   consistency (`stopped` | `crash` | `application`), optional `quiesceRef`. Status:
   phase, started and finished, size, **consistency actually achieved**, `bundleID`,
   K3s version, architecture, checksum, `verified`, error. Includes the K3s server token
   and node credentials by name in its manifest. Cold and stopped-VM first (1a);
   guest LVM or QEMU block snapshots for running-cluster points later (1b, which
   changes the appliance).
3. **`HostBackupSchedule`**. Cron or interval, retention (last N, daily, weekly),
   destination, allowed downtime window for cold points. Status: last success, next
   run, missed runs, objective breached since, battery or offline reason.
4. **`HostQuiesceHook`**. A workload registers pre/post hooks (CNPG checkpoint,
   CARE write fence) used only for `application` consistency. The host calls them,
   times them out and records the outcome. Appliance-level points are never
   described as PITR.
5. **Recovery kit status.** `HostRecoveryKit`: manifest present, key export
   verified, location, last verification. Key export itself stays in the launcher UI.
6. **`HostRestorePlan` and `HostRestoreDrill`**. Plan: feasibility, expected data
   loss, dependencies (architecture, exact appliance, free space, key available).
   Drill: restore to a fresh state directory, boot isolated with outbound side effects
   blocked, run named checks, report evidence and elapsed time (RTO). **Cutover and
   fencing of the old instance are launcher-UI approvals only.**
7. **Restore-precedence report.** When both an appliance rollback and a later
   application backup exist, status says which is newer so CNCM can warn.

### P2: lifecycle and operations

| # | Request | Why |
| --- | --- | --- |
| 8 | **Autostart and crash recovery** (`HostStatus.autostart`, request to enable). Launcher starts at login or boot; guest recovers after power loss. | A 03:00 reboot that leaves the clinic down is the most common outage. |
| 9 | **Graceful power requests.** `HostPowerRequest` (`shutdown` or `restart-appliance`) with launcher approval, plus keep-awake setting. | CNCM can drain CARE before host maintenance. |
| 10 | **Appliance update hand-off.** Surface `UPDATES.md` state (prepared, activating, candidate-running) and a request to prepare a candidate; commit stays operator-attested in the launcher. | One place shows host and CARE update status. |
| 11 | **Bounded guest logs on the data disk** and a read-only `HostLogs` summary. | Forensics survives reboot (the journal is volatile today). |
| 12 | **Image-store and disk accounting.** Size of containerd images, local-path volumes and Kite DB, with a safe "prune unused images" request that CNCM can trigger. | `care_clinic`'s Storage page. |
| 13 | **Egress and DNS for Tailscale** or a documented recipe for a Tailscale workload with LAN-less access. | Home access for doctors. |

### P3: later

Gateway API only if needed; Windows firewall helper for 80/443 and UDP 5353;
off-device cloud adapters; multi-device recovery-kit transfer; cross-machine drills.

## Acceptance CNCM will ask for

- Workload-SA create, update and delete, and RBAC and admission rejection, for every
  new CRD, mirroring the publication suite.
- Interruption at each recovery-point and restore phase never reports partial success.
- Restore on a second machine from kit plus separately held key; drill evidence recorded.
- Destination removed, full or offline shows `Degraded` with reason; schedule keeps
  the last verified point.
- Writer credentials cannot delete or shorten retention (ransomware-style test).

## What stays in CNCM

PostgreSQL PITR (CNPG and Barman), Silo/object backups, CARE settings and plugins,
CARE updates and rollback, restore of CARE state, user-facing status. The appliance
supplies destinations, host recovery points, and truthful host status.
