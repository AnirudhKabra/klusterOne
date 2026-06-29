# klusterOne (`ko-controller`)

A Kubernetes operator + `kubectl` plugin for running an operator-supplied
script on a set of nodes - safely. Each run is a `NodeMaintenance` (`nm`)
custom resource: pick the script, pick the nodes, pick a rollout budget, and
the controller handles cordon → script → uncordon for you.

## What's in the box

- **CRD** `nodemaintenances.ko.io` (cluster-scoped, short name `nm`)
- **Controller** `ko-controller` - reconciles NM objects
- **CLI** `kubectl-nm` - `create`, `attach`, `pause`, `run`, `status`, `logs`

## Quickstart

Build, deploy, install the CLI:

```bash
make docker IMAGE=ko-controller:dev          # build the controller image
minikube image load ko-controller:dev        # push it to the cluster
make deploy IMAGE=ko-controller:dev          # install CRD + controller + RBAC

make cli                                     # build the kubectl plugin
sudo install -m 0755 bin/kubectl-nm /usr/local/bin/kubectl-nm
```

> For a cloud cluster, swap `minikube image load` for a real registry push
> (`docker push ghcr.io/you/ko-controller:tag`) and use that image in
> `make deploy IMAGE=...`. To install the CLI without `sudo`, run
> `make install-cli DEST=$HOME/.local/bin`.

Run something:

```bash
kubectl nm create write-name --all-nodes --paused
kubectl nm attach write-name ./scripts/test-script.sh
kubectl nm run    write-name
```

`--paused` defers the script - `attach` swaps it in, `run` kicks off the
rollout. This is the recommended workflow because it lets you author and
re-author the script without recreating the NM. Pass `--script PATH` to
`create` directly if you prefer a one-shot.

Watch it work:

```bash
kubectl get nm                      # phase, targets, done/total
kubectl nm status write-name        # per-node table
kubectl nm logs   write-name -f     # stream runner-pod stdout
minikube ssh -- cat /ko/name.txt    # verify the side effect on the node
```

Tear down:

```bash
kubectl delete nm --all             # otherwise CRD deletion blocks on finalizers
make undeploy
```

## Documentation

| Topic                                          | Page                                             |
|------------------------------------------------|--------------------------------------------------|
| Hands-on cluster walkthrough                   | [docs/exploring.md](./docs/exploring.md)         |
| Full `kubectl-nm` reference                    | [docs/cli.md](./docs/cli.md)                     |
| Threat model + admission lockdown              | [docs/security.md](./docs/security.md)           |
| Components & data flow                         | [docs/architecture.md](./docs/architecture.md)   |
| Reconcile lifecycle (3-node walkthrough)       | [docs/reconcile-flow.md](./docs/reconcile-flow.md) |
| How Script runs on the host                    | [docs/script-action.md](./docs/script-action.md) |
| Full CRD spec + codegen workflow               | [docs/crd-reference.md](./docs/crd-reference.md) |

## Layout

```
api/         CRD Go types
cmd/         manager + kubectl-nm binaries
config/      CRD, RBAC, Deployment, samples
docs/        deep-dive references
internal/    controller, orchestrator, actions, CLI logic
scripts/     example node scripts
```
