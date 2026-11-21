# Integration tests

Go tests that run dranet against a single node [kind](https://kind.sigs.k8s.io/)
cluster. They sit between the unit tests and the `bats` e2e suite: real
apiserver, scheduler, resourceclaim controller, kubelet and containerd/NRI,
but no image build, no `kind load` and no DaemonSet rollout. This is the
place for scheduling and lifecycle ordering tests, that need the real control plane and kubelet timing.

```
make integration-test
# or
go test -tags=integration -v -count=1 ./tests/integration/...
```

## How it works

`TestMain`:

1. builds a static `dranet` binary on the host;
2. creates a single node kind cluster (or reuses one with the same name);
3. copies the binary into the node container and starts it as a transient
   systemd unit (`dranet`) with `--kubeconfig=/etc/kubernetes/admin.conf`,
   then waits for the node's ResourceSlice;
4. creates the `dra.net` DeviceClass.

Each test gets its own namespace and creates dummy links on the node with
`docker exec`. Pods use the pause image already on the node, so nothing is
pulled. On failure the dranet journal for the test window and the pods,
events, claims and slices are written to a per-run directory under
`_artifacts/integration/`.

Tests must not call `t.Parallel()`: they share the node's network namespace
and the single dranet process.

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `DRANET_KIND_CLUSTER` | `dranet-integration` | kind cluster name. An existing cluster with this name is reused and kept. |
| `DRANET_KIND_REUSE` | unset | Keep the cluster after the run. The next run skips cluster creation and only rebuilds and restarts dranet. |
| `DRANET_KIND_IMAGE` | the image in `kind.yaml` | kind node image. |
| `DRANET_BINARY` | unset | Use this prebuilt binary instead of running `go build`. |
| `DRANET_ARGS` | unset | Extra flags for the dranet process, for example `--feature-gates=PersistentResourceSliceAttributes=true`. |
| `ARTIFACTS` | `_artifacts/integration` | Where failed tests write logs and object dumps. |

## Iterating

```
export DRANET_KIND_REUSE=1
go test -tags=integration -v -count=1 -run TestFastReschedule ./tests/integration/
# edit code, run again: rebuild + restart dranet takes a few seconds
kind delete cluster --name dranet-integration   # when done
```

Useful commands against the node while a test runs or after a failure:

```
docker exec dranet-integration-control-plane journalctl -u dranet -f
docker exec dranet-integration-control-plane ip -br addr
docker exec dranet-integration-control-plane systemctl restart dranet
```

## Writing a test

Call `newEnv(t)` to get a namespace that is cleaned up (pods first, then the
links the test created, then the namespace) and dumped on failure. Use
client-go and `wait.PollUntilContextTimeout` directly for the Kubernetes
side; the only shared pieces are node queries (`addDummyLink`, `addDummyLinks`, `hostLink`,
`deviceByIfName`, `tc.nodeExec`) and driver control (`withDranetArgs` to run
one test with extra flags, `tc.restartDranet` for crash and restart
scenarios).
