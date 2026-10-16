# CNCM development

CNCM is a single-machine, single-CARE-instance proof of concept. Read README.md
before changing lifecycle behavior.

## Architecture

- Go HTTP backend, React/TypeScript frontend embedded into the binary.
- SQLite owns CNCM accounts, sessions, settings, release and operation history.
- Plain YAML under manifests/ is the deployment source of truth. Parse YAML and
  change explicit fields; do not add Helm, Kustomize, a templating DSL, or a CRD.
- BuildKit builds; Distribution stores images; CNPG manages PostgreSQL.
- Only ohcnetwork/care and ohcnetwork/care_fe, develop branches, are supported.
- Pin resolved commits and record immutable image digests for every build.

## Working rules

- Keep the implementation concrete and small. Prefer standard libraries and
  Kubernetes primitives to additional controllers or frameworks.
- Never use the ambient kubeconfig context. Local execution requires an explicit
  CNCM_KUBE_CONTEXT. Never deploy to a production cluster for a test.
- Do not connect to or test workloads on any cluster until the user explicitly
  authorizes that target. Use local tests and fake Kubernetes clients meanwhile.
- Do not deploy, change node/containerd configuration, or destroy data without
  explicit authorization for the target cluster.
- Preserve secrets, PVCs, database clusters and image recovery candidates.
- Serialize mutations. Persist operation stages before executing side effects.
- After interruption, report uncertain operations; never silently rerun migrations.
- A successful CNPG backup is a database backup, not a whole-clinic recovery point.
- Do not expose upgrades/restore as supported until their recovery paths are tested.
- Never log passwords, tokens, kubeconfigs, Secret data or unredacted environment.
- Keep authorization and same-origin checks server-side. UI controls are not security.
- Keep README capability claims consistent with actual behavior and verification.
- Follow https://careui.ohc.network/ for UI: Figtree, Geist Mono, emerald primary,
  neutral surfaces, compact accessible controls and 10px radii. Bundle fonts locally.

## Checks

Run `make check` (frontend typecheck/build, Go formatting check, vet and tests).
Use `make build` for the embedded binary. Focus tests on authentication,
validation, operation serialization/recovery and rendered manifest contracts.
Live end-to-end acceptance requires an explicitly selected disposable k3s cluster.
Do not create commits or push unless requested.
