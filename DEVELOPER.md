# Developer documentation


## Develop locally

Use [KIND](https://kind.sigs.k8s.io/)

1. Create kind cluster with the recommended config

```
make kind-cluster
```

2. Do your changess to the codebase and rollout the custom version to the kind
   cluster

```
make kind-install
```

3. Test your changes locally, use the [examples folder](./examples) for dropping manifests
and README of the scenarios you are testing.

4. Once finish the development add an e2e test in bash using the `bats`
   framework in [the tests folder](./tests)

You can run your tests locally using `bats tests/`


## Develop in a cluster


1. Build and push the image to a registry

```
docker build . --tag aojea/dranet:test --push
```

2. Install dranet

```
kubect apply -f ./install.yaml
```

3. When developing new features update the image of the `dranet` daemonset and
   rollout

```sh
$ kubectl set image ds/dranet -n kube-system dranet=aojea/dranet:test
daemonset.apps/dranet image updated
$ kubectl rollout status ds/dranet -n kube-system
Waiting for daemon set "dranet" rollout to finish: 1 out of 5 new pods have been
updated...
```

Set the ImagePullPolicy to Alwasy to always get the latest changes

```
kubectl patch daemonset dranet -n kube-system --type='strategic' -p='
{
  "spec": {
    "template": {
      "spec": {
        "containers": [
          {
            "name": "dranet",
            "imagePullPolicy": "Always"
          }
        ]
      }
    }
  }
}'
```

If you build new images just restart the ds to pull it

```
kubectl -n kube-system rollout restart ds dranet
daemonset.apps/dranet restarted
```

## Runtime provider

A runtime provider (`site/content/docs/concepts/runtime-provider.md`), gated
behind `--feature-gates=RuntimeProvider=true`, configures a Pod's network
namespace between DRANET's `RunPodSandbox` and the start of the Pod's first
container, with its own timeout. The pieces:

* `pkg/ocihook`: the channel between the OCI hook process and the daemon. The
  hook process is `dranet oci-hook`, dispatched in `cmd/dranet` before any flag
  or daemon component is touched, and it only speaks a one-line protocol over a
  UNIX socket. The package has no Kubernetes dependencies and is tested against
  a real socket.
* `pkg/runtimeprovider`: the daemon-side OCI hook manager (`*runtimeprovider.Manager`).
  Owns host executable resolution (`Synchronize`), pending container hooks,
  once-per-sandbox completion tracking, and ordered invocation of configured
  `cloudprovider.RuntimeProvider`s.
* `pkg/driver/runtime_provider.go`: thin driver glue connecting `NetworkDriver`'s
  NRI/DRA hooks, `PodConfigStore` checkpoint persistence (`RuntimeProviderDone`
  and `ResourceClaim` in `DeviceConfig`), Prometheus metrics, and Kubernetes
  events to `*runtimeprovider.Manager`.
* `cloudprovider.RuntimeProvider`: the provider contract, implemented by the
  webhook provider as `POST /ConfigurePod` behind the `runtimeProvider` capability.

The container runtime executes hooks on the host, where DRANET's executable
path inside its image means nothing, so the hook path is `/proc/<pid>/exe` of
the daemon with the PID in the runtime's PID namespace:
* **With `hostPID: true`** (or when running directly on the host): the daemon
  is in the initial PID namespace (recognized by the kernel's fixed inode
  `PROC_PID_INIT_INO` on `/proc/self/ns/pid`), so `os.Getpid()` is used
  directly at startup without depending on NRI `Synchronize` or container
  entrypoint arguments.
* **Without `hostPID: true` (default)**: the daemon finds its own container in
  NRI `Synchronize`, matched by Pod UID (from the kubelet Pod directory in
  `/proc/self/mountinfo`) and arguments, and uses the host PID reported by the
  runtime. Keep the DRANET container running the `dranet` executable directly:
  a wrapper changes the arguments and the PID the runtime reports.

Package initializers of the daemon's dependencies run in the hook process too
(Go runs every `init` before `main`); they must stay cheap and quiet, since the
runtime reports the hook's stderr in the container start error. Today the AWS
Neuron lookup logs one warning on non-EFA instances (see
https://github.com/aws-neuron/connected-device-maps-over-efa-for-neuron/pull/3).

Validation, from unit to end to end:

```sh
go test ./pkg/ocihook ./pkg/runtimeprovider ./pkg/cloudprovider/webhook -count=1
go test ./pkg/driver -run 'RuntimeProvider|ConfigurePod|ClaimSnapshot' -count=1
go test ./cmd/dranet -run 'TestRuntimeProviders|TestHookSubcommand' -count=1  # runs the built binary as the runtime does
bats tests/python_webhook.bats  # kind: success and ordering, persistent failure, recovery through the kubelet retry
```

## Checkpoint database: upgrades and rollbacks

DRANET stores device state (`DeviceConfig`) in a node-local bbolt database
(`--db-path`, default `/var/run/dranet/dranet.db`) so state like DHCP leases
survives daemon restarts. The database uses bbolt's file lock and is local to
each node.

The database layout version is stored under the `meta/schemaVersion` key
(`checkpointSchemaVersion` in `pkg/driver/pod_device_config_bolt.go`). Databases
without a `meta` bucket were created before versioning and are treated as version 1.

Migrations run sequentially (for example, v1 -> v2 -> v3 -> v4). All migrations
and the version update run in a single transaction. If any migration step fails
(for example, 3 -> 4), the entire transaction rolls back to the starting
version (v1). This prevents leaving the database in an intermediate version that
an older version of DRANET cannot read.

### Changing the checkpoint format

`TestDeviceConfigWireFormatGolden` validates the serialized JSON data. When making changes:

1. **Additive changes**: New optional fields (`omitempty`) do not require a
   version bump. Older versions ignore unknown fields. Update `fullDeviceConfig()`
   and update the golden test data if needed.
2. **Breaking changes**: Bump `checkpointSchemaVersion` and add an entry in
   `checkpointMigrations` for breaking changes (renaming fields, changing types,
   or modifying bucket layout).

### Upgrade and rollback behavior

| Scenario | Behavior |
|---|---|
| Upgrade with additive changes | Existing entries load with new fields unset. |
| Upgrade with schema bump | Migrations run when opening the database. If a migration fails, the entire transaction rolls back. |
| Rollback within same schema | The daemon reads existing data and ignores unknown fields. Unmodified entries keep the unknown fields on disk. |
| Rollback across schema bump | The daemon fails to start because it cannot read a newer schema version. Recover by rolling forward to the newer version, or delete the database file if discarding checkpoint state is acceptable. |
| Overlapping pods during rolling update | bbolt allows only one process to hold the database file lock. The new pod waits up to 1 second for the lock, then fails and restarts until the old pod exits. |

Every scenario is covered by tests in `pkg/driver/pod_device_config_bolt_version_test.go`.

## Troubleshooting

```
kubectl -n kube-system get pods -l app=dranet -o wide
NAME           READY   STATUS             RESTARTS         AGE   IP              NODE                                            NOMINATED NODE   READINESS GATES
dranet-9z66b   0/1     CrashLoopBackOff   12 (4m54s ago)   42m   10.146.104.1
```

Git commit is in the first line of logging

```
kubectl -n kube-system logs dranet-9z66b
Defaulted container "dranet" out of: dranet, enable-nri (init)
I0520 09:21:02.486329 1027992 app.go:181] dranet go go1.24.3 build: 3058756228b78265819e96963afae4dfd9497849 time: 2025-05-19T22:57:49Z
I0520 09:21:02.486404 1027992 app.go:75] FLAG: --add_dir_header="false"
I0520 09:21:02.486409 1027992 app.go:75] FLAG: --alsologtostderr="false"
I0520 09:21:02.486411 1027992 app.go:75] FLAG: --bind-address=":9177"
I0520 09:21:02.486413 1027992 app.go:75] FLAG: --filter="attributes[\"dra.net/type\"].StringValue  != \"veth\""
I0520 09:21:02.486415 1027992 app.go:75] FLAG: --hostname-override=""
I0520 09:21:02.486417 1027992 app.go:75] FLAG: --kubeconfig=""
I0520 09:21:02.486418 1027992 app.go:75] FLAG: --log_backtrace_at=":0"
I0520 09:21:02.486423 1027992 app.go:75] FLAG: --log_dir=""
I0520 09:21:02.486424 1027992 app.go:75] FLAG: --log_file=""
I0520 09:21:02.486425 1027992 app.go:75] FLAG: --log_file_max_size="1800"
I0520 09:21:02.486427 1027992 app.go:75] FLAG: --logtostderr="true"
I0520 09:21:02.486429 1027992 app.go:75] FLAG: --one_output="false"
I0520 09:21:02.486430 1027992 app.go:75] FLAG: --skip_headers="false"
I0520 09:21:02.486435 1027992 app.go:75] FLAG: --skip_log_headers="false"
I0520 09:21:02.486436 1027992 app.go:75] FLAG: --stderrthreshold="2"
I0520 09:21:02.486440 1027992 app.go:75] FLAG: --v="4"
I0520 09:21:02.486442 1027992 app.go:75] FLAG: --vmodule=""
I0520 09:21:02.486599 1027992 envvar.go:172] "Feature gate default state" feature="ClientsAllowCBOR" enabled=false
I0520 09:21:02.486609 1027992 envvar.go:172] "Feature gate default state" feature="ClientsPreferCBOR" enabled=false
I0520 09:21:02.486611 1027992 envvar.go:172] "Feature gate default state" feature="InformerResourceVersion" enabled=false
I0520 09:21:02.486614 1027992 envvar.go:172] "Feature gate default state" feature="InOrderInformers" enabled=true
I0520 09:21:02.486616 1027992 envvar.go:172] "Feature gate default state" feature="WatchListClient" enabled=false
I0520 09:21:02.491702 1027992 draplugin.go:486] "Starting"
I0520 09:21:02.491855 1027992 nonblockinggrpcserver.go:88] "GRPC server started" logger="dra"
I0520 09:21:02.491919 1027992 nonblockinggrpcserver.go:88] "GRPC server started" logger="registrar"
time="2025-05-20T09:21:04Z" level=info msg="Created plugin 00-dra.net (dranet, handles RunPodSandbox,StopPodSandbox,RemovePodSandbox)"
I0520 09:21:04.492764 1027992 app.go:157] driver started
I0520 09:21:04.492786 1027992 driver.go:430] Publishing resources
time="2025-05-20T09:21:04Z" level=info msg="Registering plugin 00-dra.net..."
I0520 09:21:04.493135 1027992 cloud.go:38] running on GCE
time="2025-05-20T09:21:04Z" level=info msg="Configuring plugin 00-dra.net for runtime containerd/1.7.24..."
```
