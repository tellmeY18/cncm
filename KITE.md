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

## Fastest path: Containers Anywhere 0.2.0 + Kite

Containers Anywhere 0.2.0 no longer preinstalls CNCM; Kite is the entry point. Two
steps install and publish CNCM with HTTPS on your LAN:

1. In Kite, open the YAML editor and apply the single file
   **https://raw.githubusercontent.com/tellmeY18/cncm/main/manifests/cncm-containers-anywhere.yaml**
   (it holds the manager plus its `WorkloadPublication`).
2. Wait for the `cncm` pod to become Ready, then open **https://cncm.cncm-system.local**
   (or **https://cncm.cncm-system.localhost** on the same computer; add `:8443` if the
   launcher fell back from port 443). Your browser warns until you trust the appliance's
   local CA: download its public `/root.crt` from the HTTP address, verify it with
   your administrator, and trust it.

No Secret needs to be authored. On first start CNCM creates Secret `cncm-bootstrap`
with a random token; in Kite open **Secrets → cncm-system → cncm-bootstrap → token**,
copy it into the setup form, and create the administrator. To choose the token
yourself, create that Secret (key `token`, 24+ characters) before applying. The
token is never logged. CARE then defaults to `https://care.care.local` and is
published the same way when installed.

On Containers Anywhere 0.3.0+, CNCM also trusts its image registry through the host API
and adds a **Host** screen; on 0.2.0 the registry trust remains a manual node step.

CNCM needs the first-boot internet access Containers Anywhere 0.2.0 already requires.
The generic steps below are for clusters without Containers Anywhere's publication API.

## Other clusters

### 1. Apply the manager manifest in Kite

Open Kite's YAML apply/editor facility for the intended cluster and paste the
contents of this file (or import its URL if your Kite version supports that):

**https://raw.githubusercontent.com/tellmeY18/cncm/main/manifests/cncm.yaml**

Review and apply it. This creates `cncm-system`, the manager's service account and
cluster-admin binding, PVC, Deployment, and Service. CNCM generates its own bootstrap
Secret on first start unless you create one first (next step).

Apply only this file, not the entire `manifests/` directory. Keep one replica.

### 2. Bootstrap Secret (optional)

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

### 3. Open CNCM at a dedicated browser origin

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

On Containers Anywhere older than 0.2.0, applying YAML in Kite installs CNCM but
does not make it reachable from your host; use the 0.2.0 fast path above.
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

## Older Containers Anywhere builds

The `care-mvp` line preinstalled CNCM with `CNCM_ORIGIN=auto` and an **Open CNCM**
button that prefilled the token. Containers Anywhere 0.2.0 (`develop`) does not;
use the fast path above.
