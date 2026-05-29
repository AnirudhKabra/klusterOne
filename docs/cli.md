# `kubectl-nm` CLI reference

`kubectl-nm` is a kubectl plugin that wraps the common workflows for
authoring, attaching scripts to, pausing, and inspecting `NodeMaintenance`
objects. It never talks to the controller directly — every command is a
plain API-server write. For where it sits in the bigger picture, see
[architecture.md](./architecture.md).

## Subcommands

```text
kubectl nm create <name> [flags]                  build & apply a NodeMaintenance
kubectl nm attach <name> <script>                 patch spec.script.inline on an existing NM
kubectl nm pause  <name> [--reason TEXT]          pause an in-flight NM (flips spec.paused=true)
kubectl nm run    <name>                          unpause an existing NM (flips spec.paused=false)
kubectl nm status <name>                          pretty-print phase + per-node table
kubectl nm logs   <name> [--node X] [-f]          stream runner-pod logs
kubectl nm push   <local> <remote> [targets]      copy a local file onto nodes
kubectl nm pull   <remote> <local> --node X       copy a node file back to local
```

Both `pause` and `run` are idempotent — re-running them when the NM is
already in the target state prints a friendly message and does not issue a
patch. `pause --reason "..."` stamps the operator-supplied reason onto the
NM as the `ko.io/pause-reason` annotation, which `kubectl nm status`
surfaces; `pause` without `--reason` and `run` both clear that annotation.

## `kubectl nm create` flags

| Flag                  | Meaning                                                              |
|-----------------------|----------------------------------------------------------------------|
| `--script PATH`       | Read script from a file; body is placed in `spec.script.inline` on the NM |
| `--inline STR`        | Use `STR` as the script body (mutually exclusive with `--script`)    |
| `--all-nodes`         | Target every node in the cluster                                     |
| `--at-once`           | Run on all targeted nodes in parallel (overrides `--max-unavailable`)|
| `--max-unavailable N` | Maximum nodes in-flight (default 1)                                  |
| `--selector k=v,…`    | Label selector for target nodes                                      |
| `--nodes a,b,c`       | Explicit node names                                                  |
| `--cordon` / `--uncordon` | Wrap the script with Cordon/Uncordon actions (both default true). Disable with `--cordon=false` / `--uncordon=false`, or the aliases `--no-cordon` / `--no-uncordon`. |
| `--drain`             | Insert a Drain action between Cordon and Script                      |
| `--timeout DURATION`  | Per-node script execution timeout (default 10m)                      |
| `--image IMG`         | Runner container image (default `alpine:3.19`)                       |
| `--in-pod`            | Run the script inside the runner Pod (skip `nsenter` to host)        |
| `--paused`            | Create paused; flip with `kubectl nm run`. Also makes `--script`/`--inline` optional (use with `kubectl nm attach`). |
| `--dry-run` / `-o`    | Print the generated NodeMaintenance YAML and exit                    |

See [script-action.md](./script-action.md) for what `--in-pod` and
`--image` actually change inside the runner Pod.

## Two-phase workflow (attach → run)

When `--paused` is set, both `--script` and `--inline` are optional — the
NM is created with an empty `spec.script.inline` and `attach` fills it in
later. Without `--paused`, one of the two flags is still required
(otherwise an empty no-op script would silently "succeed" on every
targeted node).

```bash
# Create the NM in paused mode — no script body required yet.
kubectl nm create rolling-patch --selector role=worker --paused

# Drop in (or replace) the real script later.
kubectl nm attach rolling-patch ./scripts/01.sh

# Kick it off.
kubectl nm run rolling-patch
```

`attach` is a JSON-merge patch on `spec.script.inline`. The NM's
`metadata.generation` bumps and the change shows up in API audit logs
alongside every other spec mutation — re-scripting is auditable, not
silent.

### Runner namespace and script storage

The script `ConfigMap` always lives in `ko-system`, alongside the runner
Pod. This is a fixed convention — there is no `--namespace` flag and the
runner namespace cannot be overridden at run time.

The CLI **never writes the ConfigMap directly**. The script body lives
on `spec.script.inline` of the NM CR; the controller renders it into
`nm-<name>-script` on the first reconcile and re-syncs on every change
to `spec.script.inline`.

Pause gates **execution**, not **rendering**: the CM is materialized
even when `paused: true`, so during the `create --paused` → `attach` →
`run` review window the rendered script body is inspectable *before*
the runner Pod ever launches:

```bash
kubectl get cm -n ko-system nm-<name>-script -o yaml
```

This keeps the trust surface narrow:

- operators only need `nodemaintenances.ko.io` RBAC to run scripts —
  no `configmaps.update` in `ko-system`;
- the controller's ServiceAccount is the only principal that may write
  script ConfigMaps; the `ValidatingAdmissionPolicy` in
  `config/admission/configmap_lockdown.yaml` enforces that at
  admission.

See [security.md](./security.md) for the threat model and the policy
detail.

The materialized ConfigMap carries an `ownerReference` back at the NM, so
`kubectl delete nm <name>` cascades to it (and to any `nm-<name>-output`
CM written by `pull`) via Kubernetes garbage collection.

## Halting an in-flight run

```bash
# Stop admitting new nodes and stop starting new actions. Whatever
# action is currently mid-flight on a node finishes; nothing new starts.
kubectl nm pause rolling-patch --reason "investigating node-7"

# Inspect / fix / re-attach a new script as needed
kubectl nm status rolling-patch
kubectl nm attach rolling-patch ./fixed.sh

# Resume from wherever each node left off
kubectl nm run rolling-patch
```

`pause` is a fence between actions, not an interrupt mid-action: a Script
Pod already running on a node will finish (success or fail); the pause
prevents the *next* action (e.g. Uncordon) and prevents other Pending
nodes from being admitted. To hard-stop a running script, additionally
delete the runner Pod after pausing.

The exact semantics of "fence between actions" come from the orchestrator's
one-action-per-Step invariant — see
[architecture.md](./architecture.md#what-happens-in-one-reconcile) and
[reconcile-flow.md](./reconcile-flow.md) for the underlying state machine.

## File copy (`push` / `pull`)

Both commands generate a one-shot `NodeMaintenance` with a single `Script`
action — no cordon, drain, or uncordon. The CLI waits for completion and
(by default) deletes the NM afterward. Pass `--keep` to inspect it.

### `kubectl nm push <local-path> <remote-path>`

Writes `<local-path>` onto every targeted node at `<remote-path>` (must be
absolute). The file is base64-encoded into the runner script; the script
decodes it into a temp file on the node and atomically renames into place.
Mode defaults to the local file's mode (`stat` of `<local-path>`).

```bash
# Drop a kubelet config onto every worker.
kubectl nm push ./kubelet.conf /etc/kubernetes/kubelet.conf \
  --selector node-role.kubernetes.io/worker=

# Specific nodes, custom mode, keep the NM for inspection.
kubectl nm push ./hook.sh /usr/local/bin/hook \
  --nodes node-1,node-2 --mode 0755 --keep
```

### `kubectl nm pull <remote-path> <local-path>`

Reads `<remote-path>` from a single node back to your laptop at
`<local-path>`. The runner-pod script base64-encodes the file between
sentinels and writes it to stdout. The controller copies the pod's full
stdout into a dedicated `ConfigMap` (`nm-<name>-output` in `ko-system`)
*before* deleting the pod; the CLI then reads that CM and decodes the
payload locally. The output CM is owned by the NM, so it's GC'd
automatically — no orphaned payloads. `--node` is required.

```bash
kubectl nm pull /var/log/audit.log ./audit.log --node node-1
```

### Limits

- **~700 KiB per file** in both directions. The script lives in a ConfigMap
  (1 MiB API limit), and base64 inflates the payload by ~33%.
- **Binary safe**: base64 round-trip preserves any bytes, including NULs.
- Anything larger needs a different transport (e.g. a future `FileSync`
  action that mounts a hostPath volume into the runner Pod).
