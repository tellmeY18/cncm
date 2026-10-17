# Install CNCM through Kite

The manager is packaged as a single multi-architecture image:
**`ghcr.io/tellmey18/cncm:edge`** (Linux AMD64 and ARM64).

The [publishing workflow](https://github.com/tellmeY18/cncm/actions/workflows/publish.yaml)
checks the code before building each main-branch image. `edge` tracks the latest
successful build; `sha-<full-commit>` tags and the workflow's image digest let you
pin a specific build. On the first publication, the repository owner must make
the GHCR package public in its settings before anonymous pulls work.

## What you need

- Kite connected to your explicitly approved cluster, with administrator access.
- A default StorageClass and 1 GiB for CNCM's persistent state.
- Access from the node to GHCR. No Docker build on your workstation is needed.

The **manager** uses ordinary Kubernetes resources. The **CARE installation**
workflow still requires one Linux node, privileged BuildKit Jobs, sufficient build
capacity, and local-registry runtime configuration. See [DEPLOYMENT.md](DEPLOYMENT.md).

## 1. Apply the manager manifest in Kite

Open Kite's YAML apply/editor facility for the intended cluster and paste the
contents of this file (or import its URL if your Kite version supports that):

**https://raw.githubusercontent.com/tellmeY18/cncm/main/manifests/cncm.yaml**

Review and apply it. This creates `cncm-system`, the manager's service account and
cluster-admin binding, PVC, Deployment, and Service. The pod waits for the
bootstrap Secret from the next step; `CreateContainerConfigError` is expected
until that Secret exists.

Apply only this file, not the entire `manifests/` directory. Keep one replica.

## 2. Create the bootstrap Secret in Kite

In namespace **`cncm-system`**, create a Secret with:

| Field    | Value                                                  |
| -------- | ------------------------------------------------------ |
| Name     | `cncm-bootstrap`                                       |
| Type     | `Opaque`                                               |
| Data key | `token`                                                |
| Value    | A newly generated random token, at least 24 characters |

Use a password manager to generate and retain a 64-character random token. If
using Kite's raw YAML editor, place it under `stringData.token` so Kubernetes
handles encoding. A form that edits `data` directly expects base64; follow Kite's
field guidance. Never reuse an example/shared token. Keep the Secret on subsequent
installs/restarts; it is still referenced by the Deployment.

The pod should now start. Inspect its status and logs in Kite. Do not use
**Prepare infrastructure** yet unless the target is approved for CARE workloads.

## 3. Open CNCM at a dedicated browser origin

Use a **workstation-local** port-forward to Service `cncm`, remote port **8080**,
local port **8080**, if your Kite version provides one. Otherwise, on a workstation
with explicit kubeconfig access:

```sh
kubectl --context "your-approved-context" -n cncm-system \
  port-forward --address 127.0.0.1 svc/cncm 8080:8080
```

Open **http://localhost:8080**, enter the bootstrap token, and create your CNCM
administrator. The portal now works; no CARE workloads are installed automatically.

**Kite's GET-only Kube Proxy is not suitable for CNCM login or mutation requests.**
A port-forward run inside a Kite pod terminal is local to that pod, not your
workstation. CNCM also needs its own origin, not a path beneath Kite's origin.

If Containers Anywhere is your only access to the cluster and provides neither
host kubeconfig nor workload forwarding, applying YAML in Kite installs CNCM but
does not make it reachable from your host. That access depends on
[Containers Anywhere issue #2](https://github.com/tellmeY18/containers_anywhere/issues/2).
An existing ingress can also expose CNCM at its own HTTPS origin; set
`CNCM_ORIGIN` accordingly as described in [DEPLOYMENT.md](DEPLOYMENT.md#different-port-or-an-existing-https-ingress).

## 4. Continue in CNCM

The Setup screen guides you through preflight, infrastructure preparation, image
builds, and initial CARE installation. Registry HTTP/trust settings remain a
node-administrator step before CARE can pull locally built images. Backups,
restore, and safe upgrades remain unimplemented in this POC.

## Publishing and updates

The repository owner should check the workflow completed and set the GHCR package
visibility to **Public** under the package's settings after the first push.
Public repository visibility alone does not guarantee public package visibility.

Reapplying the same manifest does not automatically restart an existing pod when
`edge` changes. To update intentionally, select a published `sha-<full-commit>` tag
or digest in Kite's Deployment editor while no CNCM operation is running. Preserve
the PVC and Secret; do not uninstall/reinstall the namespace to update the image.

Image publication and local checks do not establish live-cluster acceptance.
No workload testing is performed without explicit permission for the target.

## Preinstalled with Containers Anywhere

Recent Containers Anywhere builds ship CNCM already running (`CNCM_ORIGIN=auto`).
Choose **Open CNCM** in its control panel: the bootstrap token is filled in for you,
and the CARE address defaults to the matching `care.care.local` name. Steps 1–3
above are only needed on other clusters.
