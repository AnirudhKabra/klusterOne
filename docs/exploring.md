# Exploring & operating klusterOne

A hands-on cheat sheet for verifying an install, running a sample, observing
what the controller does node-by-node, and iterating on the controller image.
Assumes the controller has been deployed via `make deploy` — see the
[top-level README](../README.md) for install.

## 1. Verify the install

```bash
kubectl get crd nodemaintenances.ko.io                  # CRD applied
kubectl -n ko-system get deploy ko-controller-manager   # controller up
kubectl -n ko-system logs   deploy/ko-controller-manager --tail=50
```

Field-level schema docs: `kubectl explain nodemaintenance.spec` (drill into
`spec.script`, `spec.strategy`, `status.nodes` as needed).

The controller's effective permissions:

```bash
kubectl auth can-i --list \
  --as=system:serviceaccount:ko-system:ko-controller-manager \
  | grep -iE 'node|pod|configmap|nodemaint'
```

## 2. Run a sample

Bundled `NodeMaintenance` examples live in `config/samples/`:

| File                        | What it does                                                  |
|-----------------------------|---------------------------------------------------------------|
| `nodemaintenance.yaml`      | Cordon → Script → Uncordon on every node                      |
| `all-at-once.yaml`          | Same, but all nodes in parallel (`atOnce: true`)              |
| `labeled-workers.yaml`      | Uses a `nodeSelector` instead of `--all-nodes`                |
| `host-script.yaml`          | Wraps an existing host script (edit `HOST_SCRIPT_PATH` first) |

```bash
kubectl apply -f config/samples/nodemaintenance.yaml
kubectl get nm -w                          # watch live phase + done/total
kubectl nm status example-script           # per-node table
kubectl nm logs   example-script -f        # tail runner-pod stdout
```

Or author an ad-hoc run from a local script:

```bash
kubectl nm create demo --all-nodes --paused
kubectl nm attach demo ./scripts/test-script.sh
kubectl nm run    demo
```

Full CLI reference: [cli.md](./cli.md).

## 3. Observe what the controller spawns

For each in-flight node the controller materializes a **runner Pod** plus a
**script ConfigMap** in `ko-system`, and patches the Node for
cordon/drain/uncordon:

```bash
kubectl -n ko-system get pods,configmaps              # runners + their CMs
kubectl -n ko-system logs <runner-pod>                # script stdout/stderr
kubectl get nodes                                     # SchedulingDisabled flips
kubectl events --for nodemaintenance/example-script   # NM-scoped events
```

The runner pod's name is recorded in `status.nodes[*].scriptPodName`, so
`kubectl nm logs <nm> --node <node>` can stream it without a listing step.

## 4. Control a run

| Action      | CLI                                  | Raw kubectl                                                            |
|-------------|--------------------------------------|------------------------------------------------------------------------|
| Pause       | `kubectl nm pause <name> [--reason]` | `kubectl patch nm <name> --type=merge -p '{"spec":{"paused":true}}'`   |
| Resume      | `kubectl nm run <name>`              | `kubectl patch nm <name> --type=merge -p '{"spec":{"paused":false}}'`  |
| Swap script | `kubectl nm attach <name> <path>`    | Edit `nm-<name>-script` ConfigMap directly                             |
| Edit spec   | —                                    | `kubectl edit nm <name>`                                               |
| Delete      | —                                    | `kubectl delete nm <name>`                                             |

`pause` is a fence between actions, not an interrupt — a Script Pod already
running on a node finishes first. See
[cli.md → Halting an in-flight run](./cli.md#halting-an-in-flight-run).

## 5. Iterate on the controller image (minikube)

`make docker` compiles `bin/ko-controller` on the host first, then assembles
a distroless runtime image around it — so `docker build` does not need
network access to pull a Go builder or download modules.

```bash
make docker IMAGE=ko-controller:dev
minikube image load ko-controller:dev
kubectl -n ko-system rollout restart deploy/ko-controller-manager
kubectl -n ko-system rollout status  deploy/ko-controller-manager
```

For single-node minikube you can also build directly into the cluster's
docker daemon with `eval $(minikube docker-env)`, skipping the load step.
`minikube image load` works for both single- and multi-node profiles, so
the snippet above is the safer default.

If your minikube has no internet, pre-load the default runner image:

```bash
minikube image load alpine:3.19
```

## 6. Debug

- **`kubectl get nm` columns blank** → the controller hasn't reconciled yet.
  Check `kubectl -n ko-system logs deploy/ko-controller-manager`.
- **Single richest debug view** → `kubectl get nm <name> -o yaml`. The
  `.status.nodes[*]` array carries per-node `action`, `phase`, `startedAt`,
  `finishedAt`, `runnerPod`, `scriptExitCode`, and `message`.
- **`ImagePullBackOff` on the controller pod** → the `ko-controller:dev`
  image isn't visible to the cluster runtime. Re-run §5. Confirm with
  `kubectl -n ko-system describe pod <pod>` (look at the `Events` section).
- **CRD delete hangs** → there are still `NodeMaintenance` objects.
  `kubectl delete nm --all` first.
- **CRD schema changes** → after editing
  `api/v1alpha1/nodemaintenance_types.go`:
  ```bash
  make manifests && make generate && kubectl apply -k config
  ```

## 7. Tear down

```bash
kubectl delete nm --all       # otherwise CRD deletion blocks on finalizers
make undeploy
```
