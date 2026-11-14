# VFIO

This example gives a Pod a whole PCI function through VFIO. See
[VFIO](https://dranet.sigs.k8s.io/docs/concepts/vfio/) for how it works.

## Requirements

* The node has an IOMMU enabled and the `vfio-pci` module loaded:

  ```sh
  ls /sys/kernel/iommu_groups | head
  ls /sys/bus/pci/drivers/vfio-pci
  ```

* DRANET runs with `--feature-gates=VFIODriverRebind=true`. With Helm:

  ```sh
  helm upgrade --install dranet deployments/helm/dranet --set args.featureGates=VFIODriverRebind=true
  ```

## Find the functions that can be given through VFIO

Functions that are safe to bind to `vfio-pci` have `dra.net/vfio: true`. The
other attributes describe the function on its kernel driver:

```sh
kubectl get resourceslices -o json | jq -r '
  .items[].spec.devices[]
  | select(.attributes["dra.net/vfio"].bool == true)
  | [.name, .attributes["resource.kubernetes.io/pciBusID"].string, .attributes["dra.net/ifName"].string]
  | @tsv'
```

## Run the Pod

```sh
kubectl apply -f examples/vfio/vfio.yaml
kubectl logs vfio-pod
```

The Pod lists `/dev/vfio/<group>` and `/dev/vfio/vfio`. On the node, the
allocated function is now bound to `vfio-pci`:

```sh
readlink /sys/bus/pci/devices/<pciBusID>/driver
```

The claim status reports `Ready` with reason `VFIODeviceReady`:

```sh
kubectl get resourceclaims -o yaml | grep -A4 conditions
```

## Release the function

```sh
kubectl delete -f examples/vfio/vfio.yaml
```

DRANET binds the original driver back, and the function is published again
with its network interface attributes.
