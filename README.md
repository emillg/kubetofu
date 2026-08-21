# kubetofu

**kubetofu** is a Kubernetes operator that manages [OpenTofu](https://opentofu.org/) — the open-source, MPL-licensed alternative to Terraform — declaratively. You describe *what* infrastructure you want in a `TofuModule` custom resource, and the operator runs OpenTofu to plan and apply it, stores state in Secrets, and re-checks for drift on a schedule.

It's a GitOps-style workflow for OpenTofu, running inside your cluster instead of on a laptop or in CI:

```
TofuModule (spec) ──► plan ──► approve ──► apply ──► monitor for drift
                        │            │          │
                        └── Kubernetes Job running the tofu-runner image
```

## Features

- **Two-phase workflow with manual approval**: a plan is generated first and applied only after you set `spec.approvePlan: true`
- **Module sources**: public git repositories or inline ConfigMaps (private-repo credentials are on the roadmap — see Known limitations)
- **State management**: state, plans, and outputs are stored in Kubernetes Secrets (sensitive data never leaks into the resource status)
- **Drift detection**: re-plans periodically (`spec.interval`, default 10m); detected drift invalidates the previous approval and must be re-approved
- **Flexible backends**: S3, GCS, AzureRM, etc. via `spec.backend`, or in-cluster state by default
- **Per-module tuning**: override the runner image, ServiceAccount, resources, and env vars via `spec.runner`

## How it works

The controller watches `TofuModule` resources and drives each one through a state machine (`status.phase`):

| Phase | Meaning |
| --- | --- |
| `Pending` | Queued; no run started yet |
| `Planning` | A `tofu plan` run is in progress |
| `PlanGenerated` | A plan exists for the current spec and awaits approval |
| `Applying` | A `tofu apply` run is in progress |
| `Applied` | The current spec was successfully applied |
| `Failed` | The last run failed |
| `Suspended` | Reconciliation is paused (`spec.paused: true`) |

Each run executes as a Kubernetes Job in the module's namespace, running the `tofu-runner` image. The runner clones the module source, runs `tofu init` + `tofu plan` (or `tofu apply` with the saved plan), and writes results back to Secrets. Failed run Jobs are **kept** (rather than deleted) so their pod logs remain available for debugging — they are cleaned up by the Job TTL (10 minutes) and replaced by a retry after the configured interval:

- **State** — `tofu-<module>-state` (key `terraform.tfstate`)
- **Plan** — `tofu-<module>-plan-<hash>` (keys `plan.tfplan`, `plan.txt`)
- **Outputs** — `tofu-<module>-outputs` (key `outputs.json`)

Module outputs from the last successful apply are also surfaced in `status.outputs` (sensitive outputs are reported by name only).

Status conditions (`PlanGenerated`, `ApplySucceeded`, `Ready`) provide machine-readable signal, e.g. for GitOps tooling or alerts.

## Prerequisites

- A Kubernetes cluster (1.25+ — the API uses CEL validation) and `kubectl` configured
- [kustomize](https://kustomize.io/) (v5+)
- Go 1.26+ and [Podman](https://podman.io/) (only if building from source) — the Makefile build targets accept any container tool via `CONTAINER_TOOL`

## Installation

### Option 1: Install from source (development / custom image)

Build and push the controller image, then deploy with kustomize:

```bash
export IMG=<registry>/kubetofu:latest

# build and push the manager image
make docker-build docker-push CONTAINER_TOOL=podman IMG=$IMG

# optionally build and push the runner image (used by run Jobs)
make docker-build-runner docker-push CONTAINER_TOOL=podman RUNNER_IMG=ghcr.io/kubetofu/tofu-runner:latest

# deploy CRDs, RBAC, and the manager into your cluster
make deploy IMG=$IMG
```

This deploys the controller into the `kubetofu-system` namespace.

### Option 2: Single-file installer bundle

Generate a consolidated `dist/install.yaml` (CRDs + RBAC + Deployment) and apply it:

```bash
make build-installer IMG=<registry>/kubetofu:latest
kubectl apply -f dist/install.yaml
```

This is convenient for scripting or distributing to clusters without kustomize.

### Verify the installation

```bash
kubectl get pods -n kubetofu-system
# NAME                                         READY   STATUS    RESTARTS   AGE
# kubetofu-controller-manager-7f8b9c5d6-abcde  1/1     Running   0          2m

kubectl get crd tofumodules.tofu.kubetofu.io
```

## Quick start

Apply the sample module. The sample ships with an inline module (a trivial
`main.tf` in a ConfigMap) so it works out of the box with no external
dependencies — no git clone, no provider downloads:

```bash
kubectl apply -f config/samples/tofu_v1alpha1_tofumodule.yaml
```

Watch it progress through its lifecycle:

```bash
kubectl get tofumodules -w
# NAME               PHASE           AGE
# tofumodule-sample  Planning        5s
# tofumodule-sample  PlanGenerated   30s
```

The module is now **waiting for approval**. The generated plan is stored in a Secret (`tofu-tofumodule-sample-plan-<hash>`). Inspect it:

```bash
kubectl get secret -l kubetofu.io/module=tofumodule-sample
```

Approve and apply:

```bash
kubectl patch tofumodule tofumodule-sample --type merge -p '{"spec":{"approvePlan":true}}'
```

```bash
kubectl get tofumodule tofumodule-sample -o wide
# NAME               PHASE     AGE
# tofumodule-sample  Applied   2m
```

Once `Applied`, the controller re-plans on the configured `spec.interval` to detect drift. If drift is found, the phase moves to `PlanGenerated` again and the previous approval is invalidated — set `spec.approvePlan: true` to apply the changes.

### Try it with a real module

The sample module is intentionally trivial (a single `output` block). Point it
at your own OpenTofu configuration, either by replacing the ConfigMap contents
with your `.tf` files or by switching the module source to git:

```yaml
apiVersion: tofu.kubetofu.io/v1alpha1
kind: TofuModule
metadata:
  name: my-infra
  namespace: default
spec:
  module:
    git:
      url: https://github.com/my-org/my-infra
      ref: main
      # subPath: modules/eks
  variables:
    - name: region
      value: eu-west-1
  approvePlan: true        # set to false for a manual-approval workflow
  interval: 10m
```

## Configuration reference

### `spec.module` (required)

Exactly one of `git` or `configMapRef` must be set.

| Field | Description |
| --- | --- |
| `git.url` | Git repository URL, e.g. `https://github.com/org/repo` (required) |
| `git.ref` | Branch, tag, or commit SHA to check out (defaults to the remote default branch) |
| `git.subPath` | Directory inside the repository containing the module root |
| `git.secretRef` | **Not yet implemented** — placeholder for git credentials |
| `configMapRef.name` | Name of a ConfigMap whose keys are the module's `.tf` files |

### `spec.variables`

Input variables rendered into a `terraform.tfvars.json`. Each variable has either a literal `value` (any JSON value) or a `valueFrom`:

```yaml
variables:
  - name: region
    value: us-east-1
  - name: api_token
    valueFrom:
      secretKeyRef:
        name: provider-creds
        key: api_token
```

`valueFrom` supports `secretKeyRef` and `configMapKeyRef`.

### `spec.backend`

Configure a remote state backend. When omitted, state is stored in the in-cluster Secret:

```yaml
backend:
  type: s3
  config:
    bucket: my-tfstate
    key: demo.tfstate
    region: us-east-1
```

The `config` is passed to OpenTofu via `-backend-config`.

### `spec.approvePlan`

`false` by default. When `true`, the controller applies the plan generated from the current spec. It is idempotent — keeping it `true` has no effect until the spec changes and a new plan is generated.

### `spec.runner`

Customize the pod that executes OpenTofu:

```yaml
runner:
  image: my-registry/tofu-runner:1.0      # overrides the default runner image
  serviceAccountName: my-runner-sa        # use your own SA instead of the auto-provisioned one
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
  env:
    - name: HTTP_PROXY
      value: http://proxy.example.com:3128
```

If you supply your own `serviceAccountName`, you are responsible for granting it permission to read/write the module's Secrets. Otherwise the controller provisions a `tofu-runner` ServiceAccount with scoped Secret permissions in the module's namespace.

The controller-wide default runner image can be set with the `--runner-image` flag on the manager (it defaults to `ghcr.io/kubetofu/tofu-runner:latest`).

### `spec.interval`

How often the controller re-plans to detect drift (e.g. `10m`, `1h`). Defaults to `10m`. Set to `0` to disable drift detection.

### `spec.paused`

Set to `true` to suspend reconciliation. The module's phase becomes `Suspended` and no runs are started.

## RBAC and security

- The controller runs with a ClusterRole scoped to `TofuModule`, Jobs, Secrets, ConfigMaps, ServiceAccounts, and Roles/RoleBindings.
- Runner pods use a namespace-scoped ServiceAccount (`tofu-runner`) with permission to read/write Secrets only.
- State, plans, and outputs live in Secrets — never in the resource spec/status.
- Controller and runner pods adhere to the restricted Pod Security Standards (run as non-root, read-only root filesystem, no privilege escalation).

## Development

```bash
make test        # unit tests (uses envtest: real kube-apiserver + etcd)
make lint        # golangci-lint
make run         # run the controller locally against your current kubeconfig context
make test-e2e    # e2e tests against an isolated Kind cluster
```

Regenerate CRDs / RBAC / DeepCopy after API changes:

```bash
make manifests generate
```

**Do not hand-edit generated files** — `config/crd/bases/*.yaml`, `config/rbac/role.yaml`, and `**/zz_generated.*.go` are produced by `make manifests generate`.

## Local testing with kind and Podman

This guide runs the full workflow — build, deploy, and run the sample module — on a local [kind](https://kind.sigs.k8s.io/) cluster using [Podman](https://podman.io/) instead of Docker. All Makefile image targets accept `CONTAINER_TOOL=podman`, and kind is pointed at Podman via `KIND_EXPERIMENTAL_PROVIDER=podman`.

### 1. Prerequisites

- **Podman 4.0+** (`podman --version`)
- **kind v0.20+** (`kind version`)
- **kubectl**
- **Go 1.26+** — to build the manager and runner images from source

> **macOS / Windows**: Podman runs inside a VM. On first use run `podman machine init`, then `podman machine start` (start it again after reboots). Verify with `podman info`.

### 2. Create the kind cluster

kind auto-detects its container runtime, but when both Docker and Podman are installed it prefers Docker. Force Podman explicitly:

```bash
export KIND_EXPERIMENTAL_PROVIDER=podman
kind create cluster --name kubetofu
```

Verify that kind used Podman:

```bash
kubectl cluster-info
podman ps   # should list a kubetofu-control-plane container
```

### 3. Build the images with Podman

```bash
make docker-build CONTAINER_TOOL=podman IMG=example.com/kubetofu:v0.0.1
make docker-build-runner CONTAINER_TOOL=podman RUNNER_IMG=example.com/kubetofu/tofu-runner:v0.0.1
```

Use a **non-`:latest` tag**: for `:latest` images the kubelet defaults to `imagePullPolicy: Always` and will try to pull from a registry instead of using the image you load into kind.

### 4. Load the images into the cluster

```bash
podman save -o /tmp/kubetofu-manager.tar example.com/kubetofu:v0.0.1
kind load image-archive /tmp/kubetofu-manager.tar --name kubetofu

podman save -o /tmp/kubetofu-runner.tar example.com/kubetofu/tofu-runner:v0.0.1
kind load image-archive /tmp/kubetofu-runner.tar --name kubetofu
```

### 5. Deploy the operator

```bash
make deploy IMG=example.com/kubetofu:v0.0.1
```

This installs the CRDs, RBAC, and the manager Deployment into `kubetofu-system` (it only uses kubectl + kustomize, so the container tool is irrelevant). Verify:

```bash
kubectl get pods -n kubetofu-system
# kubetofu-controller-manager-7f8b9c5d6-abcde   1/1   Running   0   2m
```

### 6. Point run Jobs at the locally built runner image

The manager defaults to `ghcr.io/kubetofu/tofu-runner:latest`, which is not the image you built. Set the image per module in the sample before applying it:

```yaml
# config/samples/tofu_v1alpha1_tofumodule.yaml
spec:
  runner:
    image: example.com/kubetofu/tofu-runner:v0.0.1
```

or patch the running sample later:

```bash
kubectl patch tofumodule tofumodule-sample --type merge \
  -p '{"spec":{"runner":{"image":"example.com/kubetofu/tofu-runner:v0.0.1"}}}'
```

### 7. Run the quick start

```bash
kubectl apply -f config/samples/tofu_v1alpha1_tofumodule.yaml
kubectl get tofumodules -w
```

Then follow [Quick start](#quick-start) to approve and apply. Skipping step 6 shows up as `ErrImagePull` on the run Jobs.

### 8. Debugging

```bash
kubectl logs -n kubetofu-system deployment/kubetofu-controller-manager -c manager -f
kubectl get jobs -o wide
kubectl describe pod -l job-name=<run-job>
```

Failed run Jobs are **not** deleted by the controller — they stay around for the Job TTL (10 minutes) so you can inspect the failed pod's logs:

```bash
kubectl logs -n default pod/<failed-run-pod> --previous
```

### 9. Clean up

```bash
kind delete cluster --name kubetofu
podman machine stop   # macOS / Windows
```

### Alternative: run the e2e test suite with Podman

`make test-e2e` builds and loads images with your container tool (it reads `CONTAINER_TOOL` and falls back to Podman when Docker is absent — see `test/utils/utils.go`) and runs against its own isolated `kubetofu-test-e2e` cluster:

```bash
export CONTAINER_TOOL=podman
export KIND_EXPERIMENTAL_PROVIDER=podman
make test-e2e
```

## Known limitations

- `spec.module.git.secretRef` is declared in the API but not yet wired into the runner — private git repos over HTTPS/SSH with credentials are not supported yet (workaround: mount credentials via the runner pod's ServiceAccount/image).
- `spec.module.git.ref` is checked out with `git clone --branch`; commit SHAs are not supported, only branches and tags.
- Plans and state are stored in Secrets, which have a 1 MiB size limit — very large plans may fail to persist.

## License

Apache License 2.0
