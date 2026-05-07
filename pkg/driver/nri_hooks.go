/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/nri/pkg/api"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	metav1apply "k8s.io/client-go/applyconfigurations/meta/v1"
	resourceapply "k8s.io/client-go/applyconfigurations/resource/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/set"
)

// NRI hooks into the container runtime, the lifecycle of the Pod seen here is local to the runtime
// and is not the same as the Pod lifecycle for kubernetes, per example, a Pod that can fail to start
// is retried locally multiple times, so the hooks need to be idempotent to all operations on the Pod.
// The NRI hooks are time sensitive, any slow operation needs to be added on the DRA hooks and only
// the information necessary should passed to the NRI hooks via the np.podConfigStore so it can be executed
// quickly.

func (np *NetworkDriver) Synchronize(_ context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	klog.Infof("Synchronized state with the runtime (%d pods, %d containers)...",
		len(pods), len(containers))

	livePods := set.New[types.UID]()
	for _, pod := range pods {
		klog.Infof("Synchronize Pod %s/%s UID %s", pod.Namespace, pod.Name, pod.Uid)
		klog.V(2).Infof("pod %s/%s: namespace=%s ips=%v", pod.GetNamespace(), pod.GetName(), getNetworkNamespace(pod), pod.GetIps())
		livePods.Insert(types.UID(pod.Uid))
		// get the pod network namespace
		ns := getNetworkNamespace(pod)
		// host network pods are skipped
		if ns != "" {
			// store the Pod metadata in the db
			np.netdb.AddPodNetNs(podKey(pod), ns)
		}
	}

	// Prune persisted configs for pods that no longer exist in the runtime.
	// This handles the case where pods were deleted while the driver was down.
	for _, storedUID := range np.podConfigStore.ListPods() {
		if !livePods.Has(storedUID) {
			klog.Infof("Synchronize: pruning stale config for pod %s", storedUID)
			np.podConfigStore.DeletePod(storedUID)
		}
	}

	return nil, nil
}

// CreateContainer handles container creation requests.
func (np *NetworkDriver) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	klog.V(2).Infof("CreateContainer Pod %s/%s UID %s Container %s", pod.Namespace, pod.Name, pod.Uid, ctr.Name)
	start := time.Now()
	status := statusNoop
	defer func() {
		nriPluginRequestsTotal.WithLabelValues(methodCreateContainer, status).Inc()
		nriPluginRequestsLatencySeconds.WithLabelValues(methodCreateContainer, status).Observe(time.Since(start).Seconds())
	}()
	podConfig, ok := np.podConfigStore.GetPodConfig(types.UID(pod.GetUid()))
	if !ok {
		return nil, nil, nil
	}

	defer func() {
		// Update container creation activity timestamp.
		klog.V(3).Infof("Pod %s hit CreateContainer for container %s, updating activity timestamp", pod.Uid, ctr.Name)
		np.podConfigStore.UpdateLastNRIActivity(types.UID(pod.GetUid()), time.Now())
	}()

	adjust, update, err := np.createContainer(ctx, pod, ctr, podConfig)
	if err != nil {
		status = statusFailed
	} else {
		status = statusSuccess
	}
	return adjust, update, err
}

func (np *NetworkDriver) createContainer(_ context.Context, _ *api.PodSandbox, _ *api.Container, podConfig PodConfig) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	// Containers only care about the RDMA char devices.
	devPaths := set.Set[string]{}
	adjust := &api.ContainerAdjustment{}

	for _, config := range podConfig.DeviceConfigs {
		for _, dev := range config.RDMADevice.DevChars {
			// do not insert the same path multiple times
			if devPaths.Has(dev.Path) {
				continue
			}
			devPaths.Insert(dev.Path)
			// TODO check the file permissions and uid and gid fields
			adjust.AddDevice(&api.LinuxDevice{
				Path:  dev.Path,
				Type:  dev.Type,
				Major: dev.Major,
				Minor: dev.Minor,
			})
		}
	}

	return adjust, nil, nil
}

func (np *NetworkDriver) RunPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	klog.V(2).Infof("RunPodSandbox Pod %s/%s UID %s", pod.Namespace, pod.Name, pod.Uid)
	start := time.Now()
	status := statusNoop
	defer func() {
		nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, status).Inc()
		klog.V(2).Infof("RunPodSandbox Pod %s/%s UID %s took %v", pod.Namespace, pod.Name, pod.Uid, time.Since(start))
		nriPluginRequestsLatencySeconds.WithLabelValues(methodRunPodSandbox, status).Observe(time.Since(start).Seconds())

	}()
	// get the devices associated to this Pod
	podConfig, ok := np.podConfigStore.GetPodConfig(types.UID(pod.GetUid()))
	if !ok {
		return nil
	}
	err := np.runPodSandbox(ctx, pod, podConfig)
	if err != nil {
		status = statusFailed
	} else {
		status = statusSuccess
	}
	return err
}
func (np *NetworkDriver) runPodSandbox(_ context.Context, pod *api.PodSandbox, podConfig PodConfig) error {
	// get the pod network namespace
	ns := getNetworkNamespace(pod)
	// host network pods can not allocate network devices because it impact the host
	if ns == "" {
		return fmt.Errorf("RunPodSandbox pod %s/%s using host network can not claim host devices", pod.Namespace, pod.Name)
	}
	// store the Pod metadata in the db
	np.netdb.AddPodNetNs(podKey(pod), ns)

	// Track all the status updates needed for the resource claims of the pod.
	statusUpdates := map[types.NamespacedName]*resourceapply.ResourceClaimStatusApplyConfiguration{}
	// Process the configurations of the ResourceClaim
	for deviceName, config := range podConfig.DeviceConfigs {
		klog.V(4).Infof("RunPodSandbox processing device: %s with config: %#v", deviceName, config)
		resourceClaim := types.NamespacedName{Name: config.Claim.Name, Namespace: config.Claim.Namespace}
		resourceClaimStatus := statusUpdates[resourceClaim]
		if statusUpdates[resourceClaim] == nil {
			resourceClaimStatus = resourceapply.ResourceClaimStatus()
			statusUpdates[resourceClaim] = resourceClaimStatus
		}
		// resourceClaim status for this specific device
		resourceClaimStatusDevice := resourceapply.
			AllocatedDeviceStatus().
			WithDevice(deviceName).
			WithDriver(np.driverName).
			WithPool(np.nodeName)

		ifName := config.NetworkInterfaceConfigInHost.Interface.Name

		// Block 1: netdev operations — only when a network interface is present.
		if ifName != "" {
			if err := attachNetdevToNS(pod, ns, deviceName, config, resourceClaimStatusDevice); err != nil {
				return err
			}
		}

		// Block 2: RDMA link device — independent of whether a netdev exists.
		// For IB-only devices (no netdev) this is the only operation here;
		// for RoCE (netdev + RDMA) it runs after the netdev block above.
		if !np.rdmaSharedMode && config.RDMADevice.LinkDev != "" {
			if err := attachRdmaToNS(config.RDMADevice.LinkDev, ns, resourceClaimStatusDevice); err != nil {
				return err
			}
		}

		// Block 3: Status conditions for IB-only devices (no netdev).
		// In exclusive RDMA mode the RDMA link was moved above; in shared mode
		// char-device injection (createContainer) is sufficient. Either way the
		// device is ready, so emit the condition unconditionally.
		if ifName == "" && config.RDMADevice.LinkDev != "" {
			resourceClaimStatusDevice.WithConditions(
				metav1apply.Condition().
					WithType("Ready").
					WithReason("RDMAOnlyDeviceReady").
					WithStatus(metav1.ConditionTrue).
					WithLastTransitionTime(metav1.Now()),
			)
		}

		resourceClaimStatus.WithDevices(resourceClaimStatusDevice)
	}
	// do not block the handler to update the status
	for claim, status := range statusUpdates {
		resourceClaimApply := resourceapply.ResourceClaim(claim.Name, claim.Namespace).WithStatus(status)
		go func() {
			ctxStatus, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := np.kubeClient.ResourceV1().ResourceClaims(claim.Namespace).ApplyStatus(ctxStatus,
				resourceClaimApply,
				metav1.ApplyOptions{FieldManager: np.driverName, Force: true},
			)
			if err != nil {
				klog.Infof("failed to update status for claim %s/%s : %v", claim.Namespace, claim.Name, err)
			} else {
				klog.V(4).Infof("updated status for claim %s/%s", claim.Namespace, claim.Name)
			}
		}()
	}

	return nil
}

// attachRdmaToNS moves the RDMA link device into the pod network namespace and
// records the RDMALinkReady status condition on resourceClaimStatusDevice.
func attachRdmaToNS(linkDev, ns string, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) error {
	klog.V(2).Infof("RunPodSandbox processing RDMA device: %s", linkDev)
	if err := nsAttachRdmadev(linkDev, ns); err != nil {
		klog.Infof("RunPodSandbox error getting RDMA device %s to namespace %s: %v", linkDev, ns, err)
		return fmt.Errorf("error moving RDMA device %s to namespace %s: %v", linkDev, ns, err)
	}
	resourceClaimStatusDevice.WithConditions(
		metav1apply.Condition().
			WithType("RDMALinkReady").
			WithStatus(metav1.ConditionTrue).
			WithReason("RDMALinkReady").
			WithLastTransitionTime(metav1.Now()),
	)
	return nil
}

// attachNetdevToNS moves the host network interface into the pod network namespace,
// applies all associated configuration (ethtool, eBPF, routes, rules, neighbors),
// and records the resulting status conditions on resourceClaimStatusDevice.
func attachNetdevToNS(pod *api.PodSandbox, ns, deviceName string, config DeviceConfig, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) error {
	ifName := config.NetworkInterfaceConfigInHost.Interface.Name
	klog.V(2).Infof("RunPodSandbox processing Network device: %s", ifName)
	// TODO config options to rename the device and pass parameters
	// use https://github.com/opencontainers/runtime-spec/pull/1271
	networkData, err := nsAttachNetdev(ifName, ns, config.NetworkInterfaceConfigInPod.Interface)
	if err != nil {
		klog.Infof("RunPodSandbox error moving device %s to namespace %s: %v", deviceName, ns, err)
		return fmt.Errorf("error moving network device %s to namespace %s: %v", deviceName, ns, err)
	}

	resourceClaimStatusDevice.WithConditions(
		metav1apply.Condition().
			WithType("Ready").
			WithReason("NetworkDeviceReady").
			WithStatus(metav1.ConditionTrue).
			WithLastTransitionTime(metav1.Now()),
	).WithNetworkData(resourceapply.NetworkDeviceData().
		WithInterfaceName(networkData.InterfaceName).
		WithHardwareAddress(networkData.HardwareAddress).
		WithIPs(networkData.IPs...),
	) // End of WithNetworkData

	// The interface name inside the container's namespace.
	ifNameInNs := networkData.InterfaceName

	// Apply Ethtool configurations
	if config.NetworkInterfaceConfigInPod.Ethtool != nil {
		err = applyEthtoolConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Ethtool)
		if err != nil {
			klog.Infof("RunPodSandbox error applying ethtool config for %s in ns %s: %v", ifNameInNs, ns, err)
			return fmt.Errorf("error applying ethtool config for %s in ns %s: %v", ifNameInNs, ns, err)
		}
	}

	// Check if the ebpf programs should be disabled
	if config.NetworkInterfaceConfigInPod.Interface.DisableEBPFPrograms != nil &&
		*config.NetworkInterfaceConfigInPod.Interface.DisableEBPFPrograms {
		err := detachEBPFPrograms(ns, ifNameInNs)
		if err != nil {
			klog.Infof("error disabling ebpf programs for %s in ns %s: %v", ifNameInNs, ns, err)
			return fmt.Errorf("error disabling ebpf programs for %s in ns %s: %v", ifNameInNs, ns, err)
		}
	}

	vrfTable := 0
	if config.NetworkInterfaceConfigInPod.Interface.VRF != nil {
		vrfTable, err = applyVRFConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Interface.VRF)
		if err != nil {
			return fmt.Errorf("error configuring VRF for device %s in ns %s: %w", deviceName, ns, err)
		}
	}

	// Configure routes
	err = applyRoutingConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Routes, vrfTable)
	if err != nil {
		klog.Infof("RunPodSandbox error configuring device %s namespace %s routing: %v", deviceName, ns, err)
		return fmt.Errorf("error configuring device %s routes on namespace %s: %v", deviceName, ns, err)
	}

	// Configure rules
	// If VRF is enabled, rules are not needed/supported as routing is handled by the VRF table + l3mdev.
	if vrfTable == 0 {
		err = applyRulesConfig(ns, config.NetworkInterfaceConfigInPod.Rules)
		if err != nil {
			klog.Infof("RunPodSandbox error configuring device %s namespace %s rules: %v", deviceName, ns, err)
			return fmt.Errorf("error configuring device %s rules on namespace %s: %v", deviceName, ns, err)
		}
	}

	// Configure neighbors
	err = applyNeighborConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Neighbors)
	if err != nil {
		klog.Infof("RunPodSandbox for pod %s/%s (UID %s) failed to apply neighbor configuration for interface %s in namespace %s: %v", pod.Namespace, pod.Name, pod.Uid, ifNameInNs, ns, err)
		return fmt.Errorf("failed to apply neighbor configuration for interface %s in namespace %s: %w", ifNameInNs, ns, err)
	}

	resourceClaimStatusDevice.WithConditions(
		metav1apply.Condition().
			WithType("NetworkReady").
			WithStatus(metav1.ConditionTrue).
			WithReason("NetworkReady").
			WithLastTransitionTime(metav1.Now()),
	)
	return nil
}

// StopPodSandbox tries to move back the devices to the rootnamespace but does not fail
// to avoid disrupting the pod shutdown. The kernel will do the cleanup once the namespace
// is deleted.
func (np *NetworkDriver) StopPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	klog.V(2).Infof("StopPodSandbox Pod %s/%s UID %s", pod.Namespace, pod.Name, pod.Uid)
	start := time.Now()
	status := statusNoop
	defer func() {
		nriPluginRequestsTotal.WithLabelValues(methodStopPodSandbox, status).Inc()
		klog.V(2).Infof("StopPodSandbox Pod %s/%s UID %s took %v", pod.Namespace, pod.Name, pod.Uid, time.Since(start))
		nriPluginRequestsLatencySeconds.WithLabelValues(methodStopPodSandbox, status).Observe(time.Since(start).Seconds())
	}()
	// get the devices associated to this Pod
	podConfig, ok := np.podConfigStore.GetPodConfig(types.UID(pod.GetUid()))
	if !ok {
		return nil
	}
	err := np.stopPodSandbox(ctx, pod, podConfig)
	if err != nil {
		status = statusFailed
	} else {
		status = statusSuccess
	}
	return err
}

func (np *NetworkDriver) stopPodSandbox(_ context.Context, pod *api.PodSandbox, podConfig PodConfig) error {
	defer func() {
		np.netdb.RemovePodNetNs(podKey(pod))
	}()
	// get the pod network namespace
	ns := getNetworkNamespace(pod)
	if ns == "" {
		// some version of containerd does not send the network namespace information on this hook so
		// we workaround it using the local copy we have in the db to associate interfaces with Pods via
		// the network namespace id.
		ns = np.netdb.GetPodNetNs(podKey(pod))
		if ns == "" {
			klog.Infof("StopPodSandbox pod %s/%s using host network ... skipping", pod.Namespace, pod.Name)
			return nil
		}
	}
	needsRescan := false
	for deviceName, config := range podConfig.DeviceConfigs {
		// Move the RDMA device back to the host namespace BEFORE the netdev.
		// nsDetachNetdev calls LinkSetUp on the VF in the host namespace, which
		// triggers a NEWLINK event causing the inventory to rescan. If the RDMA
		// device is still in the pod namespace at that point it will not be
		// detected, so it must be returned first.
		rdmaDetached := false
		if !np.rdmaSharedMode && config.RDMADevice.LinkDev != "" {
			if err := nsDetachRdmadev(ns, config.RDMADevice.LinkDev); err != nil {
				klog.Errorf("fail to return rdma device %s : %v", deviceName, err)
			} else {
				rdmaDetached = true
			}
		}

		netdevDetached := false
		ifName := config.NetworkInterfaceConfigInPod.Interface.Name
		if ifName != "" {
			if err := nsDetachNetdev(ns, ifName, config.NetworkInterfaceConfigInHost.Interface.Name); err != nil {
				klog.Errorf("fail to return network device %s : %v", deviceName, err)
			} else {
				netdevDetached = true
			}
		}

		if needsRescanAfterDetach(rdmaDetached, netdevDetached) {
			needsRescan = true
		}
	}
	if needsRescan {
		np.netdb.RequestRescan()
	}
	return nil
}

// needsRescanAfterDetach reports whether the inventory needs an explicit
// rescan after returning a device's RDMA / netdev to init_net.
//
// The netdev path's NEWLINK (emitted by nsDetachNetdev's LinkSetUp) acts as
// an implicit rescan trigger for the inventory. RDMA returns to init_net do
// not produce an event the inventory observes, so an explicit rescan is
// needed only when RDMA was successfully returned but the netdev path did
// not fire NEWLINK — that is, IB-only devices (no netdev to detach) or
// SR-IOV pods where nsDetachNetdev failed.
//
// Failure cases for the RDMA detach fall back to the inventory's periodic
// poll because the device is still in the pod namespace and a rescan now
// would not observe any state change.
func needsRescanAfterDetach(rdmaDetached, netdevDetached bool) bool {
	return rdmaDetached && !netdevDetached
}

func (np *NetworkDriver) RemovePodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	klog.V(2).Infof("RemovePodSandbox Pod %s/%s UID %s", pod.Namespace, pod.Name, pod.Uid)
	start := time.Now()
	status := statusNoop
	defer func() {
		nriPluginRequestsTotal.WithLabelValues(methodRemovePodSandbox, status).Inc()
		nriPluginRequestsLatencySeconds.WithLabelValues(methodRemovePodSandbox, status).Observe(time.Since(start).Seconds())
	}()
	if _, ok := np.podConfigStore.GetPodConfig(types.UID(pod.GetUid())); !ok {
		return nil
	}
	err := np.removePodSandbox(ctx, pod)
	if err != nil {
		status = statusFailed
	} else {
		status = statusSuccess
	}
	return err
}

func (np *NetworkDriver) removePodSandbox(_ context.Context, pod *api.PodSandbox) error {
	np.netdb.RemovePodNetNs(podKey(pod))
	return nil
}

func (np *NetworkDriver) Shutdown(_ context.Context) {
	klog.Info("Runtime shutting down...")
}

func getNetworkNamespace(pod *api.PodSandbox) string {
	// get the pod network namespace
	for _, namespace := range pod.Linux.GetNamespaces() {
		if namespace.Type == "network" {
			return namespace.Path
		}
	}
	return ""
}

func podKey(pod *api.PodSandbox) string {
	return fmt.Sprintf("%s/%s", pod.GetNamespace(), pod.GetName())
}
