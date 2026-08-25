# DRANET Helm Chart

## Installation

From a local checkout:

```sh
helm upgrade --install dranet ./deployments/helm/dranet -n kube-system
```

## Configuration

The following table lists the configurable parameters and their default values:

| Parameter | Description | Default |
|-----------|-------------|---------|
| `nameOverride` | Override the chart name | `""` |
| `fullnameOverride` | Override the full release name | `""` |
| `image.repository` | Container image repository | `registry.k8s.io/networking/dranet` |
| `image.tag` | Container image tag | `stable` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `imagePullSecrets` | List of image pull secrets | `[]` |
| `rbac.create` | Create RBAC resources | `true` |
| `podAnnotations` | Annotations to add to pods | `{}` |
| `podLabels` | Labels to add to pods | `{}` |
| `logVerbosity` | Log verbosity level | `4` |
| `metricsPort` | Port for the metrics/healthz server and readiness probe | binary default: `9177` |
| `metricsPath` | HTTP path for the startup and readiness probes | `/healthz` |
| `tolerations` | Pod tolerations | `[{operator: Exists, effect: NoSchedule}]` |
| `resources.requests.cpu` | CPU resource request | `100m` |
| `resources.requests.memory` | Memory resource request | `50Mi` |
| `resources.limits.cpu` | CPU resource limit | `""` (not set) |
| `resources.limits.memory` | Memory resource limit | `""` (not set) |
| `serviceAccount.annotations` | Annotations to add to the service account | `{}` |
| `extraVolumes` | Extra volumes, added after the built-in volumes | `[]` |
| `extraVolumeMounts` | Extra volume mounts for the dranet container, added after the built-in mounts | `[]` |
| `args.filter` | CEL expression to filter network interface attributes | see binary default |
| `args.dbPath` | Persistent bbolt database path; set to `""` to use in-memory state | binary default: `/var/run/dranet/dranet.db` |
| `args.inventoryMinPollInterval` | Minimum interval between two consecutive inventory polls | binary default: `2s` |
| `args.inventoryMaxPollInterval` | Maximum interval between two consecutive inventory polls | binary default: `1m` |
| `args.inventoryPollBurst` | Number of inventory polls that can be run in a burst | binary default: `5` |
| `args.moveIBInterfaces` | If true, InfiniBand (IPoIB) interfaces are moved into the pod network namespace | binary default: `true` |
| `args.cloudProviderHint` | Hint for the cloud provider plugin (`GCE`, `AZURE`, `OKE`, `AWS`, `ALIBABA`, `CKS`, `webhook`, `NONE`); auto-detected if unset | binary default: `""` |
| `args.profileProvider` | Provider for user profile configuration (`cloud`, `webhook`, `none`) | binary default: `cloud` |
| `args.webhookURL` | HTTP, HTTPS, or Unix socket URL; required when either provider uses `webhook` | binary default: `""` |
| `args.featureGates` | Comma-separated feature gate settings in `key=value` format | binary default: `""` |

> **Note:** All `args.*` fields are optional. When omitted, the flag is not passed to the binary and the binary's built-in default applies.

The chart mounts `/var/run/dranet` from the host so the default database survives
DRANET pod replacement. Custom database paths must be placed under that directory
to remain persistent. Set `args.dbPath` to an empty string to use in-memory state.

When upgrading from a chart that does not mount `/var/run/dranet`, the existing
container-local database cannot be copied into the new host path. Before the first
upgrade that enables this mount, stop workloads that use DRANET-managed devices.
After the upgrade, restart those workloads.

When `args.profileProvider` or `args.cloudProviderHint` is `webhook`, set
`args.webhookURL` to the webhook endpoint.

`extraVolumes` and `extraVolumeMounts` add entries after the built-in volumes
and container mounts. The chart does not change the entries. Do not reuse the
built-in volume names. Example, a host configuration directory mounted
read-only:

```yaml
extraVolumes:
  - name: host-config
    hostPath:
      path: /etc/host-config
      type: DirectoryOrCreate
extraVolumeMounts:
  - name: host-config
    mountPath: /etc/host-config
    readOnly: true
```

Parameters can be set at install time using `--set` or a custom values file:

```sh
helm upgrade --install dranet ./deployments/helm/dranet -n kube-system --set logVerbosity=6
helm upgrade --install dranet ./deployments/helm/dranet -n kube-system -f my-values.yaml
```
