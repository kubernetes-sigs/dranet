---
title: "GKE Multiple Networks"
date: 2026-06-25T00:00:00Z
---

This guide shows how to use DRANET on GKE to give Pods access to additional GCE
network interfaces through Dynamic Resource Allocation (DRA). The full manifests
are in the [demo_gke_multinetwork example](https://github.com/kubernetes-sigs/dranet/tree/main/examples/demo_gke_multinetwork).

## Create a cluster

DRANET on GKE needs multi-networking and Dataplane V2 enabled:

```sh
PROJECT="test-project"
CLUSTER="test-cluster"
ZONE="us-central1-c"
VERSION="1.34"

gcloud container clusters create "${CLUSTER}" \
    --cluster-version="${VERSION}" \
    --enable-multi-networking \
    --enable-dataplane-v2 \
    --no-enable-autorepair \
    --no-enable-autoupgrade \
    --zone="${ZONE}" \
    --project="${PROJECT}"
```

## Create a node pool with multiple networks

`dranetctl` is an opinionated tool that sets up the network infrastructure and
labels the nodes with `dra.net/acceleratorpod: "true"`:

```sh
dranetctl gke acceleratorpod create dranet1 \
    --additional-network-interfaces 2 \
    --machine-type e2-standard-16 \
    --node-count 2 \
    --cluster test-cluster \
    --location us-central1-c -v 2
```

## Install DRANET

Install DRANET on the nodes created above:

```sh
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/dranet/main/examples/dranetctl-install.yaml
kubectl get pods -l k8s-app=dranet -n kube-system
```

Once the pods are running, DRANET exposes the node interfaces as DRA devices.
Inspect them with `kubectl get resourceslices -o yaml`.

## Define a DeviceClass

The DeviceClass selects devices managed by the `dra.net` driver:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: multinic
spec:
  selectors:
    - cel:
        expression: device.driver == "dra.net"
```

## Request interfaces

There are two ways to attach an interface to a workload.

### Per-Pod claims with a ResourceClaimTemplate

A `ResourceClaimTemplate` gives each Pod its own claim. This example selects the
interface named `eth1` and attaches it to every replica of a Deployment:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: phy-interfaces-template
spec:
  spec:
    devices:
      requests:
        - name: phy-interfaces-template
          exactly:
            deviceClassName: multinic
            selectors:
              - cel:
                  expression: device.attributes["dra.net"].name == "eth1"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: server-deployment
  labels:
    app: MyApp
spec:
  replicas: 2
  selector:
    matchLabels:
      app: MyApp
  template:
    metadata:
      labels:
        app: MyApp
    spec:
      resourceClaims:
        - name: phy-interfaces
          resourceClaimTemplateName: phy-interfaces-template
      containers:
        - name: agnhost
          image: registry.k8s.io/e2e-test-images/agnhost:2.54
          args:
            - netexec
            - --http-port=80
          ports:
            - containerPort: 80
```

### A shared claim with a ResourceClaim

A single `ResourceClaim` can be referenced directly by a Pod. This example
selects an interface on the `dranet-net` cloud network:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: dranet-network
spec:
  devices:
    requests:
      - name: req-phy-interface
        exactly:
          deviceClassName: multinic
          selectors:
            - cel:
                expression: device.attributes["dra.net"].cloudNetwork.contains("dranet-net")
---
apiVersion: v1
kind: Pod
metadata:
  name: pod1
  labels:
    app: pod
spec:
  containers:
    - name: ctr1
      image: registry.k8s.io/e2e-test-images/agnhost:2.54
  resourceClaims:
    - name: dranet-network
      resourceClaimName: dranet-network
```

## Clean up

```sh
dranetctl gke acceleratorpod delete dranet1
```
