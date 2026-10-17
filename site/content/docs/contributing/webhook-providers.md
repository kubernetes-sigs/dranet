---
title: "Custom Webhook Providers (BYODP)"
weight: 5
---

## Bring Your Own DRANET Provider (BYODP)

DRANET supports a flexible webhook architecture that allows users to supply custom implementations for both hardware discovery (Cloud Provider) and user intent (Profile Provider).

Instead of hardcoding bare-metal or CNI logic directly into DRANET, you can delegate these responsibilities to an external HTTP REST server.

### Enabling Webhook Providers

To enable the webhook provider, update the `dranet` daemonset arguments:

* **`--cloud-provider-hint=webhook`**: Instructs DRANET to delegate physical/infrastructural truth (e.g., base MTU, hardware MAC, VPC Subnet IP) to the webhook.
* **`--profile-provider=webhook`**: Instructs DRANET to delegate logical network assignments and IPAM (e.g., specific IP addresses or overlays) to the webhook.
* **`--webhook-url=<url>`**: The HTTP URL of your webhook server. It supports standard HTTP/HTTPS URLs (e.g., `http://127.0.0.1:8080`) as well as Unix domain sockets (e.g., `unix:///var/run/dranet/webhook.sock`).

You can mix and match providers. For example, you can use the native GCP cloud provider for hardware discovery, but use a webhook for custom IPAM.

### Architecture Pipeline

The following diagram illustrates how DRANET communicates with the webhook providers during device discovery and profile resolution:

```mermaid
sequenceDiagram
    participant D as DRANET Daemon
    participant W as Webhook Server (BYODP)

    note over D,W: Initialization Phase
    D->>W: GET /health
    W-->>D: { "cloudProvider": true, "profileProvider": true }

    note over D,W: Cloud Provider Phase (Hardware Discovery)
    D->>W: POST /GetDeviceAttributes (Device ID)
    W-->>D: Hardware attributes (MAC, PCI, Network constraints)
    D->>W: POST /GetDeviceConfig (Device ID)
    W-->>D: Baseline Hardware Settings (MTU, baseline routes)

    note over D,W: Profile Provider Phase (User Intent Resolution)
    D->>W: POST /GetProfileConfig (Device ID, ResourceClaim, full NetworkConfig)
    W-->>D: Logical Network Config (Assigned IPs, custom routes)

    note over D: DRANET merges all configs
    note over D: DRANET programs the interface statically

    note over D,W: Teardown Phase
    D->>W: POST /ReleaseProfileConfig (Device ID, ResourceClaim UID, full NetworkConfig)
    W-->>D: 200 OK (Resources released)
```

When the webhook is also a runtime provider, one more call happens between the profile resolution and the teardown, once the Pod's network namespace exists and DRANET has attached and configured the devices in it:

```mermaid
sequenceDiagram
    participant K as kubelet / containerd
    participant R as runc
    participant D as DRANET Daemon
    participant W as Webhook Server (BYODP)

    K->>D: NRI RunPodSandbox
    note over D: DRANET attaches and configures the devices in the Pod's netns
    K->>D: NRI CreateContainer (first container)
    D-->>K: OCI spec + createRuntime hook (dranet oci-hook)
    K->>R: create container
    R->>D: hook: READY <container>
    D->>W: POST /ConfigurePod (Pod, netns, claims, devices)
    W-->>D: 200 OK (netns configured) / error
    D-->>R: OK / FAIL <reason>
    R->>K: container starts / start fails, kubelet retries
```


### How it Works in Practice

**1. Profile Providers (User Intent)**
Profile providers are options that third-party providers can expose to users to pick. The `profile` string in the DRANET configuration acts as a reference. It is suggested to follow a format that can be easily extended and clearly identified, such as `domain/name` (e.g. `acme.com/overlay`), or one with a clear meaning. In the `webhook-whereabouts` example, the `profile` string directly maps to the name of the underlying CNI configuration file.

**2. Cloud Providers (Cluster Provider Intent)**
Cloud providers are the authoritative source for the VM and its hardware, which is usually exposed via instance metadata. We offer two hooks for cloud providers: one to enhance the existing hardware metadata during discovery, and another to automate the provisioning of baseline hardware configurations. This represents the "cluster provider intent."

**3. Solid and Predictable Runtime Abstraction**
Regardless of which providers are in use, we always keep the same abstraction on the runtime for DRANET: a network interface and its associated network parameters (like IPs, routes, and rules). All of these external hooks work together to merge into a final, statically verifiable configuration for these interfaces *before* execution. This makes the DRANET runtime solid and predictable, in contrast with the dynamic behavior of standard CNI plugins that stop the world during runtime.

**4. Runtime Providers (Pod Post-Configuration)**
Some providers need to do their own plumbing in the Pod's network namespace once the devices are in it and before any workload process runs: program a fabric or SmartNIC for the Pod, load eBPF programs on the interface, run a CNI plugin, register the Pod with a controller. A runtime provider is DRANET's cloud-init for them: DRANET pauses the start of the Pod's first container, calls the webhook with the Pod, its network namespace path and its claims, and lets the container start only when the webhook answers successfully. See [Runtime Provider](../concepts/runtime-provider.md) for the mechanism and its guarantees.

### Webhook Capabilities Validation

To ensure safe delegation, DRANET requires the webhook server to declare its capabilities via the `/health` endpoint.

When DRANET connects to the webhook, it performs an HTTP `GET /health` and expects a JSON payload defining the supported capabilities:

```json
{
  "cloudProvider": false,
  "profileProvider": true,
  "runtimeProvider": true
}
```

If you start DRANET with `--cloud-provider-hint=webhook` but the webhook returns `"cloudProvider": false`, DRANET will log a fatal error during initialization to prevent misconfiguration. `runtimeProvider` is optional and independent of the other two: when `--feature-gates=RuntimeProvider=true` is enabled, DRANET uses it from any webhook configured as its cloud or profile provider.

### API Contracts

Your webhook server should implement the following HTTP `POST` endpoints based on the capabilities it provides. DRANET sends JSON payloads corresponding to its internal models.

#### Cloud Provider API (`cloudProvider: true`)

* `POST /GetDeviceAttributes`: Returns the physical hardware attributes for a device.
* `POST /GetDeviceConfig`: Returns the baseline physical network settings (like MTU).

#### Profile Provider API (`profileProvider: true`)

* `POST /GetProfileConfig`: Allocates and returns the logical profile configuration (e.g., allocating an IP address from IPAM). The request includes the complete `ResourceClaim`, including its UID, namespace, allocation, and `status.reservedFor` Pod consumer. Providers can therefore use the Claim UID as an idempotency key and the actual Pod namespace/name when integrating with an IPAM system.

  **Mutation & Validation**: The webhook receives the *entire* `NetworkConfig` (combined from user and cloud intents) as context. Unlike standard Mutating Webhooks on the API server, this node-level webhook cannot directly mutate the opaque config object in the API server. Instead, it computes and returns the *resolved profile parameters* (like the chosen IP), which DRANET then merges into the final configuration. Passing the full configuration gives the webhook the power of a Validating Admission Controller.

  **Denying Configurations**: The webhook can deny a configuration if the user's intent is invalid or conflicts with its rules. It communicates this by returning an appropriate HTTP error code. When a webhook returns a non-200 status code, DRANET aborts the network setup during the `NodePrepareResources` phase (before the pod sandbox is even created).

  **Handling Errors & HTTP Codes**:
  * **200 OK**: Request successful. Returns the allocated logical configuration.
  * **400 Bad Request**: The provided `NetworkConfig` is malformed or requests invalid parameters (e.g., requesting an IP outside the allowed subnet). The `NodePrepareResources` call will fail.
  * **404 Not Found**: The requested profile does not exist in the webhook provider.
  * **409 Conflict**: The request conflicts with current state (e.g., the statically requested IP is already in use).
  * **500 Internal Server Error**: The webhook failed to allocate resources. DRANET treats this as a temporary failure, and the kubelet will typically retry the `NodePrepareResources` call.
  * **Network Failures**: If DRANET cannot reach the webhook due to a network timeout or connection refused, it also fails the `NodePrepareResources` call, and kubelet will continually retry.

  **Trade-offs & Downsides (Node-level vs. API-level Validation)**:
  Because this validation happens at the *node level* during DRA `NodePrepareResources` (rather than at the API server via standard Admission Webhooks), there are important trade-offs to consider:
  * **Late Feedback**: If a configuration is invalid, the API server will still accept the Pod. The failure happens asynchronously when the pod is scheduled and the kubelet attempts to prepare resources. Users won't see an immediate error on `kubectl apply`; the pod will remain in a `Pending` state, and they must inspect Pod events to see the `NodePrepareResources` failure.
  * **Kubelet Retry Loops**: Standard Kubernetes behavior is to retry failed resource preparations. A persistent denial (like a 400 Bad Request) will cause the Kubelet to continuously retry `NodePrepareResources`, which can generate unnecessary load on the node and webhook server compared to an upfront API rejection.
  * **Idempotency**: The kubelet may retry `NodePrepareResources`, so DRANET can call this more than once for the same `(device, claimUID)`. It must return an equivalent result without allocating additional resources (e.g. key the allocation by `claimUID`, as `whereabouts` does via `CNI_CONTAINERID`).

* `POST /ReleaseProfileConfig`: Frees stateful resources (e.g., releasing an IP address). It receives the ResourceClaim UID and the full `NetworkConfig`. `NodeUnprepareResources` does not provide the complete ResourceClaim. The endpoint should return `200 OK` on success or if the resource was already released (idempotency).
  * **Best-effort teardown**: A failed `ReleaseProfileConfig` is logged but not retried by DRANET (teardown must not block pod deletion). The provider therefore owns leak reclamation and must be able to garbage-collect orphaned allocations on its own, otherwise resources leak permanently.

#### Runtime Provider API (`runtimeProvider: true`)

* `POST /ConfigurePod`: Called once per Pod sandbox, after DRANET attached and configured the Pod's devices in its network namespace and before the Pod's first container starts. The container runtime keeps the container paused until the call returns. The webhook configures the Pod's network namespace itself, from the node, and answers:

  ```json
  {
    "pod": {"namespace": "team", "name": "trainer-0", "uid": "6f1c..."},
    "netns": "/var/run/netns/cni-3d2a...",
    "claims": [{
      "claim": { "metadata": {"name": "nics", "namespace": "team", "uid": "9a4e..."}, "spec": {}, "status": {} },
      "devices": [{
        "name": "gpu0rdma0",
        "device": {"name": "eth1", "mac_address": "aa:bb:cc:dd:ee:01", "pci_address": "0000:00:01.0"},
        "config": {"profile": "acme.com/fabric", "interface": {"name": "net1", "addresses": ["10.0.0.5/24"]}, "routes": []},
        "rdmaDevice": "mlx5_1"
      }]
    }]
  }
  ```

  * `pod` identifies the Pod; `netns` is the path of its network namespace on the node, usable with `nsenter --net` or `setns(2)` by a provider that runs on the node with access to `/var/run/netns`.
  * `claims` lists the Pod's ResourceClaims served by DRANET, sorted by namespace and name. `claim` is the ResourceClaim as DRANET received it at `NodePrepareResources` (without `managedFields`), so the Claim UID matches the one seen by `GetProfileConfig`. Each device carries its name in the ResourceSlice, its host identifiers, the `NetworkConfig` DRANET applied in the Pod (the interface name inside the Pod is `config.interface.name`) and its RDMA link device, if any.
  * **200 OK**: the Pod is configured; the container starts. The response body is ignored.
  * **Any other status, or a network failure**: the container start fails with the response body as the reason, visible in the Pod's events (`kubectl describe pod`) and in a `RuntimeProviderFailed` event from DRANET. The kubelet retries the container with its usual backoff, and DRANET runs the hook again; the webhook decides whether a failure is permanent by failing consistently.
  * **Timeout**: the call must return within `--runtime-provider-timeout` (30s by default, up to a maximum of 60s) minus a 5s margin, after which DRANET fails the container start with a timeout reason. The container runtime kills the hook at the full timeout. Kubelet's own `--runtime-request-timeout` (2m by default) bounds the whole container start, so keep the hook timeout under it.
  * **Idempotency**: the webhook is called again on the next container creation when it fails, after a DRANET restart for Pods whose containers were not running yet, and for every new sandbox of the Pod (a new network namespace). It must be safe to repeat for the same Pod, and two attempts for the same Pod can overlap when the kubelet starts another container of the Pod while the first retries.
  * **Scope**: the webhook configures the Pod's network namespace; it must not move, rename or re-address the interfaces DRANET attached, which DRANET reports in the ResourceClaim status and detaches at teardown. Provider resources tied to the Pod are released through `ReleaseProfileConfig` at `NodeUnprepareResources`, which does not run the hook again; namespace-local state disappears with the namespace.
  * The webhook must be reachable from the DRANET daemon at the configured `--webhook-url`, as for the other calls. Calls for several Pods starting at once are made concurrently.

### Reference Implementation

An example implementation of a webhook profile provider that wraps the standard CNI `whereabouts` IPAM plugin is available at `cmd/webhook-whereabouts`. It acts as an independent module.

You can run this reference webhook locally:
```bash
cd cmd/webhook-whereabouts
go build .
./webhook-whereabouts --bind-address=127.0.0.1:8080
```
