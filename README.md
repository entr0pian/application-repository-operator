# application-repository-operator

Kubernetes operator that lets a dev lead onboard automated multi-cluster Argo
CD delivery for a repo by opening one PR with a small custom resource,
instead of the platform team hand-writing an Argo CD `Application` per repo
per cluster.

## How it fits together

```
dev lead PRs a CR into application-repositories
  → Argo CD syncs crs/*.yaml into the management cluster (application-repositories-app.yaml)
  → this operator reconciles each ApplicationRepository CR
  → commits structured keys into taskapp-argocd's apps/values*.yaml (GitHub Contents API)
  → Argo CD (already watching taskapp-argocd) renders one child Application per entry
    via apps/templates/repositories-app.yaml
```

The operator is the **single writer** into `taskapp-argocd` for
CR-onboarded repos — it never generates whole files or Application YAML
directly, only patches specific keys. It runs centrally in the management
cluster (unlike `taskapp-backend-operator`, which runs per dev/prod).

## API: `ApplicationRepository`

```yaml
apiVersion: platform.taskapp.io/v1alpha1
kind: ApplicationRepository
metadata:
  name: backend                 # also the key under taskapp-argocd's repositories.<name>
spec:
  repoURL: https://github.com/entr0pian/backend.git
  targetRevision: main          # default: main
  chartPath: chart               # default: chart
  clusters:
    - name: dev                 # selects apps/values-dev.yaml + its destinationServer
      namespace: default         # default: default
      imageTag: "c3f5e94f..."   # optional; omit to use the chart's own default tag
```

| Field | Default | Notes |
|---|---|---|
| `spec.repoURL` | — | required |
| `spec.targetRevision` | `main` | |
| `spec.chartPath` | `chart` | matches the per-repo chart-ownership convention (backend/frontend/backend-operator each own `chart/` in their own repo) |
| `spec.clusters[].name` | — | required; must match a `values-<name>.yaml` in `taskapp-argocd` |
| `spec.clusters[].namespace` | `default` | |
| `spec.clusters[].imageTag` | `""` | optional; see [imageTag semantics](#imagetag-semantics) below |

### Status

`status.conditions` — `SpecSynced` (true once `apps/values.yaml` matches
spec) and `Ready` (true only when `SpecSynced` is true **and** every
`spec.clusters[]` entry shows `committed: true`).

`status.clusters[]` — per target cluster: `name`, `committed`, `commitSHA`
(the `taskapp-argocd` commit that last updated this entry), `lastAttempt`,
`error` (set when `committed: false`, e.g. retries exhausted after repeated
409 conflicts).

## What gets written, and where

Every reconcile does two kinds of writes into `taskapp-argocd`, each a
single patched subtree — never a whole-file rewrite, never a generated
Application manifest:

1. **`reconcileSpec`** → `apps/values.yaml`: `repositories.<name>.{repoURL,targetRevision,chartPath}`. Shared across every environment.
2. **`reconcileClusters`** → `apps/values-<cluster>.yaml`, once per entry in the union of `spec.clusters` and previously-known `status.clusters` (so a cluster *removed* from spec is still visited): `repositories.<name>.enabled` (always), and — only when `enabled: true` — `.namespace` and `.imageTag`.

A cluster removed from `spec.clusters` gets `enabled: false` in its
values file, **never deleted** — Argo CD's own `prune: true` (with a
`resources-finalizer.argocd.argoproj.io` finalizer on the generated child
Application) then tears down the actual workload, and the historical record
of that cluster having been onboarded is preserved rather than erased. CR
deletion reuses this exact path: the finalizer's `handleDeletion` disables
the repo in every previously-known cluster before removing itself.

Each patch is a no-op (no commit at all) when the target file already
matches — reconciling an unchanged CR produces zero git traffic.

### `imageTag` semantics

`ImageTag` is a plain `string`, so "never set" and "cleared after being
set" are indistinguishable at reconcile time — both are the Go zero value.
One rule covers both: **empty means delete the key**, not "leave the last
value." Clearing a previously-set `imageTag` therefore reverts that
cluster's deployed image to the onboarded chart's own default tag, rather
than silently doing nothing.

### Conflict handling

Every write goes through `gitsync.PatchYAML`: fetch the file fresh, mutate,
write with a SHA compare-and-swap. On a 409 (someone else committed since
the fetch), it re-fetches and re-mutates from scratch — it never reapplies
a stale diff. Up to 5 attempts, backoff starting at 200ms and doubling each
retry. After exhausting attempts, the error is wrapped in
`gitsync.ErrConflictExhausted` and surfaces as `status.clusters[].error`
plus a non-`Ready` condition.

### Known formatting gotcha

The YAML re-serialization (`internal/yamlpatch`) does not preserve blank
lines between top-level keys in the target file — every commit that
touches a file will flatten its blank-line formatting, even though only
one key changed semantically. The change is always semantically
equivalent and Helm parses it fine; it just makes the diff noisier than
strictly necessary.

## Reconcile cadence

The operator has no event source for git-side drift — a hand-edit to
`taskapp-argocd`, or a partially-failed previous reconcile, produces no
Kubernetes watch event. Every reconcile therefore re-fetches live file
content from GitHub rather than trusting any cached state, and requeues on
a fixed interval (`REQUEUE_INTERVAL`, default 5m) to catch drift on its own.

## Configuration (environment variables)

| Var | Required | Default | Purpose |
|---|---|---|---|
| `GITHUB_TOKEN` | yes | — | write access to the `taskapp-argocd` repo; fatal at startup if unset |
| `ARGOCD_REPO_OWNER` | no | `entr0pian` | |
| `ARGOCD_REPO_NAME` | no | `argocd` | |
| `ARGOCD_REPO_BRANCH` | no | `main` | |
| `REQUEUE_INTERVAL` | no | `5m` | Go duration string (e.g. `30s`) |

## Deployment

Deployed via its own Helm chart (`chart/`), sourced directly from this
repo by `taskapp-argocd`'s `application-repository-operator-app.yaml` —
management cluster only (`.Values.applicationRepositoryOperator.enabled`
is only set `true` in `values-management.yaml`).

`GITHUB_TOKEN` is delivered via ESO: `helm-charts/platform`'s
`ExternalSecret` syncs AWS SM path `taskapp/platform/argocd-write-token`
into a `default`-namespace Secret named `argocd-write-token`
(key `token`), which `chart/templates/deployment.yaml` references
directly via `secretKeyRef`. There's no chart-managed alternative — see
`chart/values.yaml`'s `github.*` keys for the (optional) repo
owner/name/branch overrides only.

```bash
make sync-helm       # regenerate config/{crd,rbac} from kubebuilder markers,
                      # then copy them into chart/templates/
helm lint chart/
helm template --set github.token=dummy chart/   # local render smoke check
```

## Local development

```bash
go build ./...
go vet ./...
go test ./...          # envtest, uses local bin/k8s binaries (make envtest)
make manifests generate  # after any API type or +kubebuilder:rbac change
make sync-helm            # keep chart/templates/{application-repository-crd,manager-rbac}.yaml in sync
```

`test/e2e/` spins up a real kind cluster (`make test-e2e`) and deploys via
kustomize (`config/`, not the Helm chart) — this path creates its own
throwaway `github-token` Secret rather than talking to a real GitHub repo,
since it only exercises manager startup and the metrics endpoint.

## Related repos

- [`application-repositories`](https://github.com/entr0pian/application-repositories) — where dev leads PR `ApplicationRepository` CRs
- [`argocd`](https://github.com/entr0pian/argocd) (`taskapp-argocd`) — the GitOps repo this operator writes into

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
