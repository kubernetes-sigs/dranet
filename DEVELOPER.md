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

## Checkpoint database: upgrades and rollbacks

DRANET checkpoints per-device state (`DeviceConfig`) to a node-local bbolt
database (`--db-path`, default `/var/run/dranet/dranet.db`) so that state that
cannot be rebuilt from the system — for example a DHCP lease that must be
released at unprepare time — survives daemon restarts. The database is
single-writer (bbolt holds an exclusive file lock) and strictly node-local:
nodes upgrade independently and a mixed-version cluster needs no coordination.

The on-disk layout is versioned through the `meta/schemaVersion` key
(`checkpointSchemaVersion` in `pkg/driver/pod_device_config_bolt.go`). A
database without the `meta` bucket predates versioning and is treated as
version 1. Migrations run one version at a time inside a single bbolt
transaction together with the version stamp, so an interrupted migration
rolls back as a unit and is retried on the next start.

### Changing the checkpoint format

`TestDeviceConfigWireFormatGolden` pins the exact bytes on disk. When it fails
you have two options, in order of preference:

1. **Make the change additive**: new optional fields (`omitempty` pointers)
   need no version bump. Old entries load with the field unset; older daemons
   ignore the unknown key. Update `fullDeviceConfig()` and regenerate the
   golden.
2. **Bump `checkpointSchemaVersion` and add a `checkpointMigrations` entry**
   for any non-additive change (renaming, moving or retyping keys, bucket
   layout changes). Migrations are kept forever, so any old database upgrades
   directly to any newer daemon.

### Behavior matrix

| Scenario | Behavior |
|---|---|
| Upgrade, additive change | Old entries load with new fields unset. Entries are only rewritten on prepare, so mixed shapes coexist harmlessly. |
| Upgrade, schema bump | Migration chain runs at first open, atomically with the version stamp. Crash mid-migration rolls back and retries on next start. |
| Rollback, same schema | Unknown keys are ignored in memory but reads never rewrite entries, so newer state stays on disk and a re-upgrade recovers it. Only claims re-prepared while rolled back lose the newer fields. |
| Rollback across a bump | The daemon refuses to open the database (before any mutation) and crash-loops on that node. Recover by rolling forward, or delete the database file to consciously discard the state (DHCP leases then expire on their own; unprepare steps for already-prepared claims are skipped). |
| Old/new pod overlap during a rolling update | The second open fails on the file lock after a 1s timeout and the new pod restarts until the old one exits. No corruption is possible. |

Every row is pinned by a test in
`pkg/driver/pod_device_config_bolt_version_test.go`; change the behavior only
together with the corresponding test.

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
