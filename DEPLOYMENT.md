# Deploy CNCM on Kubernetes

This guide installs the **CNCM management portal** as a Kubernetes workload using
plain YAML. It needs no Helm, Kustomize, ingress controller, or cloud-specific API.
The easiest initial access is a localhost port-forward.

For the shortest dashboard-based path, use [the Kite quickstart](KITE.md).
Once the image publishing workflow has succeeded and the GHCR package is public,
the shipped manifest pulls `ghcr.io/tellmey18/cncm:edge` directly. You can skip
step 2 below and use the manifest unchanged. Building your own image remains an
option; the steps below document that path too.

The manager uses standard Kubernetes resources, but **the current CARE installer
requires exactly one Ready Linux amd64/arm64 node**. It also requires a default
StorageClass, privileged BuildKit Jobs, and node runtime access to its local HTTP
registry. Managed/restricted clusters that disallow those requirements cannot run
the complete CARE workflow as-is. This is not a claim of acceptance on every
Kubernetes distribution; live cluster acceptance is still pending.

## Prerequisites

- A disposable cluster you are authorized to use, and its explicit kubeconfig
  context. The commands below always specify it.
- Permission to create namespaces, PVCs, Deployments, Secrets, and a cluster-admin
  binding. The shipped manifest intentionally gives CNCM cluster-admin for this
  single-owner POC.
- A default StorageClass providing a filesystem volume writable by UID/GID 65532
  with `fsGroup: 65532`. The manager requests 1 GiB.
- Docker with Buildx on your build machine, or equivalent tooling that can build
  the supplied Dockerfile and publish an image to a registry your node can pull.
- Git, kubectl, and OpenSSL on your workstation.

The [publishing workflow](https://github.com/tellmeY18/cncm/actions/workflows/publish.yaml)
publishes Linux AMD64/ARM64 manager images after checks pass. Check that a build
has completed before using its tag. Publishing the CNCM image is separate from
the in-cluster registry that CNCM later creates for CARE images. Docker/Buildx is
only needed if you choose to build your own manager image.

## 1. Get the source and select the target

```sh
git clone https://github.com/tellmeY18/cncm.git
cd cncm
export KUBE_CONTEXT="your-authorized-disposable-context"
```

These commands contact the selected cluster. Run them only after target approval:

```sh
kubectl --context "$KUBE_CONTEXT" get nodes -o wide
kubectl --context "$KUBE_CONTEXT" get storageclasses
```

Confirm the intended target and a default StorageClass. If you need a specific
class for the manager, set `spec.storageClassName` on `cncm-data` in your local
manifest before applying it. The CARE installer itself still expects a default.

## 2. Build and publish the manager image

Choose a registry/repository you can push to. Authenticate with that registry
using its normal login procedure, then build for the **node's architecture**, not
necessarily your workstation's architecture:

```sh
export CNCM_IMAGE="ghcr.io/your-account/cncm:poc-$(git rev-parse --short HEAD)"
docker buildx build --platform linux/amd64 --tag "$CNCM_IMAGE" --push .
```

Use `--platform linux/arm64` for an ARM64 node, or
`--platform linux/amd64,linux/arm64` when your builder supports both. This builds
the management portal; CARE images are built later by BuildKit inside Kubernetes.

For a private image, configure an `imagePullSecret` in `cncm-system` and reference
it under `spec.template.spec.imagePullSecrets` in the manager Deployment. A
workstation's registry login does not grant image-pull access to the cluster.

For a local distribution that supports importing images directly, you can instead
build with `docker build -t cncm:0.1.0 .` and import it using that distribution's
documented command. The image must exist in the node runtime, not merely in your
workstation's Docker image store.

## 3. Create the namespace and bootstrap token

Create the namespace once; skip this command if it already exists:

```sh
kubectl --context "$KUBE_CONTEXT" create namespace cncm-system
```

Use a private temporary file so the token is not passed in kubectl arguments or
printed by these commands:

```sh
umask 077
TOKEN_FILE="$(mktemp)"
openssl rand -hex 32 > "$TOKEN_FILE"
tr -d '\n' < "$TOKEN_FILE" \
  | kubectl --context "$KUBE_CONTEXT" -n cncm-system create secret generic cncm-bootstrap \
      --from-file=token=/dev/stdin
```

Keep this terminal open and retain the file until administrator setup completes.
Open the file privately to copy its value into the setup form. Do not commit,
paste into an issue, or include the token in a screenshot. If the Secret already
exists, keep it instead of generating a replacement. You can retrieve it privately
through your usual Kubernetes Secret management tool.

## 4. Install the manager

Open `manifests/cncm.yaml` in your editor and replace the Deployment's
`image: ghcr.io/tellmey18/cncm:edge` with the exact image reference you pushed in step 2 (the value
of `CNCM_IMAGE`). Set the literal reference, not `$CNCM_IMAGE`: Kubernetes does not
expand shell variables inside YAML. Then apply the bootstrap manifest:

```sh
kubectl --context "$KUBE_CONTEXT" apply -f manifests/cncm.yaml

kubectl --context "$KUBE_CONTEXT" -n cncm-system \
  rollout status deployment/cncm --timeout=180s
```

If using the imported local image option, set `image: cncm:0.1.0` and
`imagePullPolicy: IfNotPresent`.
If you edited storage, image-pull credentials, or other fields in a local copy,
use that file in place of `manifests/cncm.yaml`.

**Apply only `manifests/cncm.yaml`.** The remaining files contain CARE image
placeholders and are applied in stages by CNCM. Do not run `kubectl apply -f
manifests/`.

The manager runs as one replica with a Recreate strategy. Do not scale it up:
SQLite and operation coordination are intentionally single-manager.

## 5. Open the portal

```sh
kubectl --context "$KUBE_CONTEXT" -n cncm-system \
  port-forward --address 127.0.0.1 svc/cncm 8080:8080
```

Open **http://localhost:8080**. Create a CNCM administrator using your bootstrap
token and a password of at least 12 characters. This account is separate from
the CARE administrator that you create during installation.

After copying/storing the token securely and finishing setup, remove its temporary
workstation file in the original terminal:

```sh
rm -- "$TOKEN_FILE"
unset TOKEN_FILE
```

Keep the Kubernetes Secret: the Deployment still references it on future starts.
Closing the port-forward disconnects the browser but does not stop CNCM or its
Jobs. Restarting the forward restores access.

### Different port or an existing HTTPS ingress

`CNCM_ORIGIN` must exactly match the browser's scheme, hostname, and port, without
a trailing slash. For example, before using localhost port 8081:

```sh
kubectl --context "$KUBE_CONTEXT" -n cncm-system set env deployment/cncm \
  CNCM_ORIGIN=http://localhost:8081
kubectl --context "$KUBE_CONTEXT" -n cncm-system rollout status deployment/cncm
kubectl --context "$KUBE_CONTEXT" -n cncm-system \
  port-forward --address 127.0.0.1 svc/cncm 8081:8080
```

For an existing ingress, route its root path to `cncm-system/cncm:8080`, terminate
TLS there, and set `CNCM_ORIGIN=https://your-manager.example`. Use a separate origin
for CARE. CNCM does not configure ingress/DNS/certificates in this cut. A change
to the Deployment restarts the manager; make it while no operation is running and
preserve the change in your local manifest for subsequent applies.

## 6. Prepare and install CARE

The following steps are for the supported **disposable single-node** setup:

1. Open **Setup** and save the clinic name and CARE browser address. For the local
   preview below, use `http://localhost:8090`.
2. Set the registry address to a node-reachable hostname/IP and port 30500. This
   is an address as seen by the node's container runtime, not your browser.
3. Run **Preflight**, then **Prepare infrastructure**. This installs the pinned
   CNPG operator, PostgreSQL, Redis, Silo, and Distribution. Preparation expects
   a fresh environment and does not adopt existing unmanaged CNPG resources.
4. Configure the node runtime for the local HTTP registry (next section).
5. Select **Build CARE images**. Backend and frontend builds run sequentially from
   pinned commits resolved from their `develop` branches.
6. In **Builds & releases**, select the completed candidate, enter the initial
   CARE administrator credentials, and choose **Install this release**.
7. Follow **Activity** and **Services** for initialization and readiness.
8. Forward CARE separately:

   ```sh
   kubectl --context "$KUBE_CONTEXT" -n care \
     port-forward --address 127.0.0.1 svc/care 8090:80
   ```

Open **http://localhost:8090**. The Caddy proxy routes API and file requests at the
same origin. This HTTP route is a local development path; TLS-dependent features
and staff-device access require the future networking integration.

### Required registry configuration

The CARE registry is currently HTTP without authentication, on NodePort 30500.
BuildKit pushes via `registry.care.svc.cluster.local:5000`; node pulls use the
registry address configured in CNCM. Both addresses must reach the same registry.

The runtime must explicitly allow HTTP for that exact registry authority. CNCM
does not write node configuration, change the host network, or restart containerd.
There is no portable Kubernetes manifest that sets this on every distribution.

For **self-managed k3s**, the cluster administrator can merge an entry like this
into `/etc/rancher/k3s/registries.yaml` on the node, replacing the example address
with the exact address saved in CNCM:

```yaml
mirrors:
  "192.0.2.10:30500":
    endpoint:
      - "http://192.0.2.10:30500"
```

`192.0.2.10` is a documentation address, not a usable default. Preserve existing
registry settings and follow your distribution's restart procedure only with
explicit node-change authorization. See the
[k3s private-registry documentation](https://docs.k3s.io/installation/private-registry).
Avoid assuming that `localhost:30500` NodePort access works on every networking
implementation; use an address known to be reachable from the node.

For other containerd distributions, use their supported registry-host
configuration. Managed clusters may prohibit HTTP registries or node changes;
the full CARE POC is not supported there without adapting this integration.
For Containers Anywhere's immutable guest, coordinate this with its maintainer
rather than editing generated node configuration ad hoc.

### Capacity and admission requirements

The full stack requests approximately **42 GiB of PVC capacity including CNCM**.
Runtime image storage, build layers, and logs need additional disk space. A build
has a 5 GiB memory limit and runs alongside PostgreSQL, CARE, and other services.
Hardware minimums have not been measured; the original 4 GiB RAM / 16 GiB disk
appliance defaults should not be assumed sufficient.

BuildKit Jobs currently require privileged containers. Clusters enforcing a
restricted Pod Security profile will reject those Jobs. Do not disable policies
on a shared cluster to work around this; use an explicitly approved isolated POC
environment. No backups or restore are implemented yet, so use disposable data.

## Troubleshooting

All diagnostic commands below target the selected cluster explicitly:

```sh
kubectl --context "$KUBE_CONTEXT" -n cncm-system get pods,pvc
kubectl --context "$KUBE_CONTEXT" -n cncm-system describe deployment cncm
kubectl --context "$KUBE_CONTEXT" -n cncm-system logs deployment/cncm --tail=100
kubectl --context "$KUBE_CONTEXT" -n care get pods,jobs,pvc
```

| Symptom                                      | Check                                                                                                      |
| -------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| CNCM PVC stays Pending                       | Default/selected StorageClass and provisioner; a WaitForFirstConsumer class needs its pod scheduled first. |
| CNCM ImagePullBackOff                        | Image was pushed/imported, correct node architecture, private-registry pull credentials.                   |
| CNCM CreateContainerConfigError              | `cncm-bootstrap` Secret exists with key `token`.                                                           |
| Read-only/permission errors under `/data`    | Volume supports the configured non-root UID/GID and fsGroup.                                               |
| POST/PUT returns origin error                | Browser URL exactly matches `CNCM_ORIGIN`; use `localhost`, not `127.0.0.1`, with the default origin.      |
| Preflight rejects node count                 | Full CARE workflow supports exactly one node.                                                              |
| Build pod rejected                           | Admission policy rejects privileged BuildKit.                                                              |
| CARE image pull says HTTPS response mismatch | Node runtime has not enabled HTTP for the configured registry address.                                     |
| Registry name cannot resolve                 | Containerd uses node DNS; Kubernetes service DNS is not automatically available to it.                     |
| Build is OOMKilled / disk pressure           | Inspect resource usage and increase disposable-cluster capacity.                                           |
| Operation is uncertain after restart         | Inspect remaining Jobs/resources in Kite first; acknowledgement only clears the operation gate.            |

Do not delete an initialization Job merely to bypass a migration guard. A failed
or interrupted initialization may already have changed the database. Expert
inspection is required; this POC does not automatically repair or replay it.

## Updating or stopping CNCM

To publish a new manager version, repeat the image build with a new immutable tag,
then update the Deployment image while no operation is running. Preserve the PVC
and bootstrap Secret; manager-state schema compatibility must be reviewed before
upgrading. Updating CNCM is different from updating CARE: CARE upgrades are not
enabled in this POC.

To stop the manager without deleting data:

```sh
kubectl --context "$KUBE_CONTEXT" -n cncm-system scale deployment/cncm --replicas=0
```

Use `--replicas=1` to start it again. This does not stop CARE or already-created
Jobs. Deleting the namespace or applying `kubectl delete -f manifests/cncm.yaml`
can delete the manager PVC; those are not data-preserving stop commands.

## Verification status

The repository has local frontend/build, Go, authentication, operation-recovery,
and fake-Kubernetes tests. This guide is based on the implemented manifests and
API paths, not a completed live-cluster acceptance run. No cluster is tested or
deployed to without explicit permission for that target.
