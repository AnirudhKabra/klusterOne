# Exploring & operating klusterOne

A hands-on cheat sheet for poking at the `NodeMaintenance` CRD, the
`ko-controller` Deployment, and individual runs. Use this after `make deploy`
to verify the install, kick off a sample, and observe what the controller
does node-by-node.

All commands assume `kubectl` points at the target cluster
(`kubectl config current-context`) and that the controller has been deployed
via `make deploy` (see the [top-level README](../README.md) for install).

## Table of contents

1. [Explore the CRD itself (API surface)](#1-explore-the-crd-itself-api-surface)
2. [Verify the controller is up](#2-verify-the-controller-is-up)
3. [Look at your nodes (the controller's targets)](#3-look-at-your-nodes-the-controllers-targets)
4. [Deploy a sample and watch it](#4-deploy-a-sample-and-watch-it)
5. [Watch what the controller spawns](#5-watch-what-the-controller-spawns)
6. [Mutate / control a run](#6-mutate--control-a-run)
7. [Use the kubectl-nm CLI](#7-use-the-kubectl-nm-cli)
8. [Tear it all down](#8-tear-it-all-down)
9. [Local minikube workflow](#9-local-minikube-workflow)
10. [Debugging tips](#10-debugging-tips)

---

## 1. Explore the CRD itself (API surface)

The CRD is **cluster-scoped**, plural `nodemaintenances`, short name `nm`,
with rich printer columns — so most commands feel like working with a
native type.

```bash
# Is the CRD installed? (apiextensions.k8s.io meta-level view)
kubectl get crd nodemaintenances.ko.io
kubectl describe crd nodemaintenances.ko.io           # versions, scope, served/storage
kubectl get crd nodemaintenances.ko.io -o yaml        # full CRD as applied

# Does the API server expose the resource?
kubectl api-resources | grep -i nodemaint             # shortname, namespaced=false, group/ver
kubectl api-versions | grep -i ko.io                  # confirms ko.io/v1alpha1 is served

# Field-by-field schema docs (server-generated from the OpenAPI schema):
kubectl explain nodemaintenance
kubectl explain nodemaintenance.spec
kubectl explain nodemaintenance.spec.script
kubectl explain nodemaintenance.spec.strategy
kubectl explain nodemaintenance.status
kubectl explain nodemaintenance.status.nodes
kubectl explain --recursive nodemaintenance.spec      # full tree, very long

# What verbs does the controller's ServiceAccount actually have?
kubectl auth can-i --list \
  --as=system:serviceaccount:ko-system:ko-controller-manager \
  | grep -iE 'node|pod|configmap|nodemaint'
```

## 2. Verify the controller is up

```bash
# Overview of everything in the controller's namespace:
kubectl -n ko-system get all
kubectl -n ko-system get deploy ko-controller-manager
kubectl -n ko-system get pods -l app.kubernetes.io/component=ko-controller
kubectl -n ko-system describe deploy ko-controller-manager

# Logs:
kubectl -n ko-system logs deploy/ko-controller-manager -f
kubectl -n ko-system logs deploy/ko-controller-manager --tail=50

# RBAC the controller has (cluster + namespace scope):
kubectl get clusterrole       ko-controller-manager-role          -o yaml
kubectl get clusterrolebinding ko-controller-manager-rolebinding  -o yaml
kubectl -n ko-system get role,rolebinding,sa

# Health and metrics endpoints (forward to your laptop):
kubectl -n ko-system port-forward deploy/ko-controller-manager 8080:8080 8081:8081
# then in another terminal:
curl localhost:8081/healthz
curl localhost:8081/readyz
curl localhost:8080/metrics
```

## 3. Look at your nodes (the controller's targets)

```bash
kubectl get nodes -o wide
kubectl get nodes --show-labels                       # useful for designing a nodeSelector
kubectl describe node <node-name>                     # see taints, conditions
```

## 4. Deploy a sample and watch it

The repo ships four example `NodeMaintenance` objects under `config/samples/`.

```bash
# Pick one to apply:
kubectl apply -f config/samples/nodemaintenance.yaml  # full Cordon→Script→Uncordon
kubectl apply -f config/samples/all-at-once.yaml      # rolls all nodes simultaneously
kubectl apply -f config/samples/labeled-workers.yaml  # uses nodeSelector
kubectl apply -f config/samples/host-script.yaml      # wraps an existing host script (edit HOST_SCRIPT_PATH first)

# Default "table" view (uses the printer columns from the CRD markers):
kubectl get nm                                        # short name
kubectl get nodemaintenance                           # full
kubectl get nodemaintenance -o wide                   # adds Pending/InProgress/Failed counts
kubectl get nm -w                                     # watch live status updates

# Single-object views:
kubectl get nm example-script -o yaml                 # full object including status
kubectl describe nm example-script                    # human-readable, includes events
kubectl get nm example-script -o jsonpath='{.status.phase}'
kubectl get nm example-script \
  -o jsonpath='{range .status.nodes[*]}{.name}{"  "}{.phase}{"\n"}{end}'

# Status subresource directly:
kubectl get --raw /apis/ko.io/v1alpha1/nodemaintenances/example-script/status | jq .
```

## 5. Watch what the controller spawns

The controller creates **runner Pods** and **ConfigMaps** in `ko-system` to
execute Script actions, and patches Nodes for Cordon / Drain / Uncordon.

```bash
# Runner pods and their ConfigMaps live in ko-system:
kubectl -n ko-system get pods -w
kubectl -n ko-system get configmaps
kubectl -n ko-system get pods --field-selector spec.nodeName=<node-name>

# Inspect a runner pod end-to-end:
kubectl -n ko-system describe pod <runner-pod>
kubectl -n ko-system logs <runner-pod>                # user script stdout/stderr
kubectl -n ko-system logs <runner-pod> -f             # tail while it runs

# Node side-effects (cordon/uncordon):
kubectl get nodes                                     # SchedulingDisabled column flips
kubectl get events --sort-by=.lastTimestamp | tail -30
kubectl get events --field-selector involvedObject.kind=NodeMaintenance

# Per-object events (kubectl >= 1.27):
kubectl events --for nodemaintenance/example-script
```

## 6. Mutate / control a run

```bash
# Pause / resume via spec.paused (the CLI exposes this as `kubectl nm pause/run`):
kubectl patch nm example-script --type=merge -p '{"spec":{"paused":true}}'
kubectl patch nm example-script --type=merge -p '{"spec":{"paused":false}}'

# Edit interactively:
kubectl edit nm example-script

# Delete the run (the controller cleans up runner pods / configmaps):
kubectl delete nm example-script
kubectl delete -f config/samples/nodemaintenance.yaml
```

## 7. Use the kubectl-nm CLI

`cmd/kubectl-nm` ships a `kubectl` plugin that wraps the common workflows
(`create --paused`, `attach`, `run`, `pause`, `logs`, `list`, …).

```bash
make install-cli                # installs kubectl-nm into /usr/local/bin (DEST= to override)
kubectl nm --help               # discover subcommands
kubectl nm list
kubectl nm get example-script
kubectl nm create my-run --all-nodes --script ./hello.sh
kubectl nm attach my-run ./hello.sh
kubectl nm run    my-run
kubectl nm pause  my-run
kubectl nm logs   my-run        # streams runner-pod logs per node
```

See [cli.md](./cli.md) for full subcommand and flag reference.

## 8. Tear it all down

```bash
kubectl delete nm --all         # otherwise CRD deletion blocks on finalizers
make undeploy                   # removes CRD + RBAC + Namespace + Deployment
# or equivalently:
kubectl delete -k config
```

## 9. Local minikube workflow

The Deployment uses `imagePullPolicy: IfNotPresent` and the Makefile defaults
`IMAGE=ko-controller:dev`, so no registry is required — but the image has to
land in minikube's container runtime.

**Option A — build directly inside minikube's docker daemon** (works for
`--driver=docker` or any driver using the docker runtime):

```bash
eval $(minikube docker-env)
make docker IMAGE=ko-controller:dev
make deploy IMAGE=ko-controller:dev
eval $(minikube docker-env -u)  # optional, restore your normal docker context
```

**Option B — build with host docker, then load** (works for any driver,
including `containerd`):

```bash
make docker IMAGE=ko-controller:dev
minikube image load ko-controller:dev
make deploy IMAGE=ko-controller:dev
```

**Iteration loop after code changes** (tag stays `:dev`, so force a re-roll):

```bash
eval $(minikube docker-env)
make docker IMAGE=ko-controller:dev
kubectl -n ko-system rollout restart deploy/ko-controller-manager
kubectl -n ko-system rollout status  deploy/ko-controller-manager
```

**Pre-load the runner image** (the controller spawns `alpine:3.19` runner
pods; pre-load it if minikube has no internet):

```bash
minikube image load alpine:3.19
```

## 10. Debugging tips

- **`kubectl get nm` columns blank** → the controller hasn't taken a step
  yet. Check controller logs (`kubectl -n ko-system logs deploy/ko-controller-manager`).
- **Single richest debugging view** → `kubectl get nm <name> -o yaml`. The
  `.status.nodes[*]` array shows per-target `action`, `phase`, `startedAt`,
  `finishedAt`, `runnerPod`, and `message` — exactly what the orchestrator
  wrote on its last step.
- **`ImagePullBackOff` on the controller pod** → the `ko-controller:dev`
  image isn't visible to the cluster's container runtime. Re-run the build
  step in [§9](#9-local-minikube-workflow). Confirm with
  `kubectl -n ko-system describe pod <pod>` (look at the `Events` section).
- **CRD schema changes** → after editing
  `api/v1alpha1/nodemaintenance_types.go`, run:
  ```bash
  make manifests   # regenerate config/crd/*.yaml from kubebuilder markers
  make generate    # regenerate api/v1alpha1/zz_generated.deepcopy.go
  kubectl apply -k config
  ```
- **`unrecognized format "int64"/"int32"` warnings on apply** → fixed in
  the `manifests` target via a post-processing strip; see the comment block
  above the target in the Makefile.
- **CRD delete hangs** → there are still `NodeMaintenance` objects.
  `kubectl delete nm --all` first.
