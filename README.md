# CNCM · Cloud Native CARE Manager

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)

A CARE-specific management portal for one machine, one Kubernetes cluster, and
one CARE installation. Go + embedded React, SQLite, and repository-owned plain
Kubernetes YAML. Inspired by Kite; styled after [CareUI](https://careui.ohc.network/).

**This is the first-install POC, not the complete lifecycle product yet. No live
cluster acceptance has been performed. Use disposable test data only.**

**Deploy on Kubernetes:** follow [the deployment guide](DEPLOYMENT.md) to build
and publish the manager image, configure its bootstrap token, apply the plain
YAML, and open the portal. The guide includes registry setup, platform constraints,
and troubleshooting for the single-node CARE workflow.

**Using Kite on Containers Anywhere 0.2.0?** Follow [the Kite quickstart](KITE.md): apply one
YAML file, copy the generated token from a Secret, and open `https://cncm.cncm-system.local`. GitHub Actions builds AMD64/ARM64
images at `ghcr.io/tellmey18/cncm:edge` and `sha-<full-commit>` after checks pass.

## What this cut implements

- Bootstrap-token-protected administrator setup; local password authentication,
  HttpOnly sessions, same-origin mutation checks, and an audit trail.
- A locally runnable portal with Overview, Setup, Builds & releases, Services,
  Backups roadmap, Activity, and Settings screens. Figtree and Geist Mono are
  bundled locally; the management UI requires no CDN fonts.
- SQLite-backed settings, operations, source commits, image digests, and release
  history. Only one mutation may run at a time.
- Explicit cluster selection. Without a cluster, authentication, settings, and
  history work; workload controls stay disabled.
- Preflight for exactly one Ready amd64/arm64 node and a default StorageClass.
- Infrastructure preparation: checksum-verified CNPG 1.30.1 installation,
  PostgreSQL, Silo, Redis, and a persistent Distribution registry.
- Sequential on-demand BuildKit Jobs for `ohcnetwork/care` and
  `ohcnetwork/care_fe`, **develop branches only**. Commits are resolved first;
  image digests are collected from build results.
- Initial CARE deployment: persistent credentials/JWKS, explicit initialization
  Job, initial administrator, API/worker/beat/frontend, and a same-origin Caddy
  proxy. Beat startup does not rerun migrations.
- Application start/stop/restart, pod status, Kubernetes events, and bounded pod
  logs. Known Secret values and common credential patterns are redacted from logs.
- Persistent uncertainty after interruption/failure. Operations are not silently
  replayed. Fresh-install guards refuse a second initialization or an uncertain
  migration retry.

These paths compile and have local tests; successful compilation is **not** proof
that upstream images build or the workload runs on a real cluster. The first
authorized disposable-cluster acceptance must establish that.

- Containers Anywhere 0.3.0 host API, used only when present (otherwise CNCM behaves
  as before): **Prepare infrastructure** asks the appliance to trust the in-cluster
  registry through `HostRegistryTrust`; a **Host** screen shows current host facts
  (memory, CPUs, disk pressure, update state, autostart, root-CA fingerprint) and lets
  the administrator send approval-gated `HostRequest`s (resize, autostart, power, image
  prune, LAN check). A person approves them in the Containers Anywhere panel. Stale or
  missing facts are shown as unknown, never replayed.

## What is not implemented

- Applying updates to an existing CARE installation or automatic rollback.
  Candidate builds are supported, but existing installations cannot be upgraded.
- S3 backup configuration, Barman Cloud Plugin installation, WAL archiving setup,
  backup schedules, file backups, recovery exports, or restore.
- Clinic/plugin configuration editors beyond clinic name, browser origin, and
  registry address. No plugin marketplace or arbitrary source repositories.
- Client certificate onboarding or production TLS. LAN publication, mDNS and local-CA
  HTTPS use the Containers Anywhere 0.2.0 `WorkloadPublication` API when present; their
  behaviour has not been verified on a live appliance.
- CNCM password recovery, multiple administrators/roles, OAuth/LDAP/MFA/passkeys,
  CNCM self-update, or uninstall/data destruction.
- Automatic registry/containerd configuration or automatic image/cache pruning.
- CPU/RAM/disk capacity enforcement, multi-node/HA, fully offline builds, or
  migration from `care_clinic`.

The Backups screen is explicitly a roadmap, not a functioning backup control.
**Persistent volumes are not backups.** Upgrades remain unavailable until recovery
of the database, Silo objects, credentials, CNCM state, and matching images is
implemented and tested together.

## Run the portal locally — no cluster access

Requires Go 1.26+, Node 22.12+ (or a current Node 24), npm, and Make. Supported
manager process platforms are Linux and macOS; Windows hosts use the Linux VM.

```sh
make build
export CNCM_BOOTSTRAP_TOKEN="$(openssl rand -hex 32)"
./bin/cncm
```

Open **http://localhost:8080** and create the administrator with the token from
your environment. Supply it only to the setup form; do not publish it in logs or
screenshots. The token must be at least 24 characters. Passwords must be 12–72
bytes and are bcrypt-hashed. Sessions expire after 12 hours.

With `CNCM_KUBE_CONTEXT` unset and no in-cluster service-account environment, CNCM
does not create a Kubernetes client and does not use the ambient current context.
This is the default preview/development mode. It has no simulated workloads or
fabricated success indicators.

SQLite lives under `data/` (ignored by version control). A process lock prevents
two managers sharing the directory. Keep one replica with the Recreate strategy.

### Environment

| Variable               | Default / meaning                                                  |
| ---------------------- | ------------------------------------------------------------------ |
| `CNCM_ADDR`            | `127.0.0.1:8080`; container sets `0.0.0.0:8080`                    |
| `CNCM_ORIGIN`          | `http://localhost:8080`; exact browser origin, no trailing slash   |
| `CNCM_DATA_DIR`        | `data`; persistent private state directory                         |
| `CNCM_BOOTSTRAP_TOKEN` | Required before the initial administrator exists                   |
| `CNCM_KUBE_CONTEXT`    | Explicit kubeconfig context for local execution                    |
| `KUBECONFIG`           | Standard kubeconfig file location; used only with explicit context |

In-cluster execution uses the pod service account when no explicit context is
configured. HTTPS origins issue Secure cookies. The server does not interpret
forwarded headers to override its configured origin.

## Cluster workflow — only after explicit target authorization

Do not test against an ambient or production context. Obtain permission for a
specific disposable single-node cluster first. Then, for local execution:

```sh
export CNCM_KUBE_CONTEXT="<explicit-disposable-context>"
./bin/cncm
```

The portal names the configured target, and every operation request must confirm
that target. This prevents accidental UI/context mismatch; it is not a substitute
for choosing the correct cluster.

1. **Setup:** save the clinic name, browser address, and node-reachable registry.
2. **Run preflight:** verifies node count/readiness/architecture and default storage.
3. **Prepare infrastructure:** installs CNPG, then registry, Redis, Silo, and DB.
4. Configure the node runtime for the local registry as described below.
5. **Build CARE images:** creates two sequential BuildKit Jobs. Follow their stages
   in Activity and inspect source/build container logs in Services.
6. **Builds & releases:** enter a separate CARE administrator username, email, and
   password, then install a built release. A temporary Secret carries administrator
   credentials to the initialization Job and is removed after successful completion.
7. **Services:** inspect readiness and use data-preserving start/stop/restart.

For a development-only browser path, on the authorized cluster:

```sh
kubectl --context "<explicit-disposable-context>" -n care port-forward svc/care 8090:80
```

The default CARE browser address is `http://localhost:8090`. Proxy routes are
`/api/*`, `/static/*`, `/ping/*`, `/health/*`, `/patient-bucket/*`, and
`/facility-bucket/*`; the frontend uses same-origin API requests. HTTP is a local
development path, not clinic LAN delivery. TLS-dependent CARE features and device
access need the future launcher integration.

### Local registry contract

On Containers Anywhere 0.3.0+ the node trust step below is automatic: CNCM creates a
`HostRegistryTrust` for the registry address (it must be `localhost` or an IP with a
port) and reports the result. On other clusters it stays a manual node-administrator
step.

The Distribution registry uses a 20 GiB PVC and NodePort **30500**, with HTTP and
no authentication for this isolated POC. BuildKit pushes to
`registry.care.svc.cluster.local:5000`. CARE images reference the address saved in
Settings, default `localhost:30500`, followed by the recorded digest.

The node's container runtime must reach that address and be explicitly configured
to permit this HTTP registry. Pod DNS resolution is not proof that containerd can
resolve a registry name. This is especially important in Containers Anywhere's
immutable guest. CNCM does **not** edit node configuration or restart containerd.
The actual endpoint/trust setup must be agreed and tested on the authorized target.
NodePort can expose registry access on node interfaces; use an isolated disposable
cluster, not an untrusted network. Authenticated TLS registry support is follow-up
work before broader exposure.

### Resources and privileges

- CNCM needs administrative access to install operators, RBAC and workloads.
- BuildKit Jobs are privileged but have no service-account token. The approved
  sources still execute upstream build code. This is a single-owner test appliance.
- Builds are sequential; each has a 5 GiB memory / 2 CPU limit and a two-hour
  deadline. Package/base-image downloads require internet.
- Requested persistent capacity is about 42 GiB including CNCM state. Actual disk
  use additionally includes runtime images, temporary build layers, and logs.
- Resource sizing is unmeasured; 4 GiB guest RAM / 16 GiB disk defaults are not a
  suitable assumption for running the stack and frontend build together.
- Jobs and previous images are retained for inspection. This cut has no automatic
  garbage collection. Review disk consumption in Kite.

Application commits and image digests are recorded. Upstream base tags and package
repositories can still change; this is build traceability, not bit-for-bit
reproducibility. Non-CARE supporting images are version-tagged, not digest-locked.

## In-cluster packaging

`Dockerfile` builds the React assets and a static Go binary, then uses a non-root
distroless runtime. Build the image with your available container build tool:

```sh
docker build -t cncm:0.1.0 .
```

`manifests/cncm.yaml` bootstraps only the manager: namespace, account, cluster-admin
binding, PVC, Deployment, and Service. Before applying it to an authorized target:

- Make the built image available to that node or replace its image reference.
- Create namespace `cncm-system` and a Secret named `cncm-bootstrap` with key
  `token`, using your normal secret-handling workflow. No shared token is shipped.
- Set `CNCM_ORIGIN` to the exact browser origin.

Then apply that file through Kite or an explicitly selected Kubernetes context.
The other manifests contain runtime image placeholders and are orchestrated by
CNCM; **do not bulk-apply the entire manifests directory**.

## Operation and recovery behavior

SQLite's unique partial index prevents concurrent or unresolved mutations. Each
stage is persisted before its side effects. After restart, previously running
operations become `uncertain`. Kubernetes Jobs may still be executing.

Inspect their state in Kite before using **Acknowledge after inspection**. That
action only clears the mutation gate. It does not prove recovery, delete Jobs,
undo changes, or authorize rerunning migrations. An existing initialization Job,
CARE API deployment, or installed/uncertain release prevents automatic reinstall.
Recovery of a partially initialized clinic currently requires expert intervention.

CNCM uses server-side apply without forcing ownership conflicts and refuses to
adopt existing resources lacking its managed-by label. A fresh dedicated cluster
is expected; an existing CNPG installation is not automatically adopted.

Logs are limited to the CARE namespace, last 150 lines / 32 KiB, and visible only
to the administrator. Redaction is best-effort; application logs can contain
clinical information. Do not use real patient data for this POC.

## Local checks

```sh
make check
make build
```

Checks include TypeScript/build, Go formatting, vet, race-enabled tests,
authentication/same-origin checks, durable operation serialization/restart
recovery, and manifest/fake-Kubernetes contracts. Tests do not discover or contact
a real cluster. No live deployment, real CARE build, Docker image build, or
end-to-end clinical acceptance has been performed as part of this implementation.

## Next milestones

1. Authorized disposable-cluster acceptance: registry pulls, both native builds,
   migrations, login, representative clinical workflow, upload/download, restart.
2. External S3 recovery: Barman base backups/WAL plus Silo objects, secrets,
   management state, and preserved release images.
3. Prove restore into a clean installation, then enable migration-safe upgrades.
4. CARE settings and reviewed plugin workflows.
5. Containers Anywhere access and host-destination integration:
   - Workload/LAN forwarding: delivered in 0.2.0 as `WorkloadPublication` (closed #2)
   - [#3: host backup destinations](https://github.com/tellmeY18/containers_anywhere/issues/3)

Local folders, USB disks, Google Drive and other consumer destinations are planned
through Containers Anywhere. No backward compatibility with `care_clinic` is
planned. Kite stays in the appliance for expert Kubernetes administration.

## License

Copyright (C) 2026 CNCM contributors.

CNCM is licensed under the **GNU Affero General Public License version 3 only**
(`AGPL-3.0-only`). See [LICENSE](LICENSE) for the full terms. CNCM is provided
without warranty, including any implied warranty of merchantability or fitness
for a particular purpose.

Source code is available at [tellmeY18/cncm](https://github.com/tellmeY18/cncm).
Modified versions offered over a network must provide their corresponding source
to users as required by section 13. Third-party dependencies retain their own
licenses.
