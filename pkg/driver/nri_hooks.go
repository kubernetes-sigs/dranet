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

	v1 "k8s.io/api/core/v1"
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

func (np *NetworkDriver) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	logger := klog.FromContext(ctx)
	logger.Info("Synchronized state with the runtime", "pods", len(pods), "containers", len(containers))

	// livePodNetNs map tracks live pods by UID and their network namespace paths.
	livePodNetNs := make(map[types.UID]string)
	for _, pod := range pods {
		podLogger := klog.LoggerWithValues(logger, "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid)
		podLogger.Info("Synchronize Pod")
		podLogger.V(2).Info("Pod network details", "netns", getNetworkNamespace(pod), "ips", pod.GetIps())
		livePodNetNs[types.UID(pod.Uid)] = getNetworkNamespace(pod)
	}

	// Process stored pods: update NetNS for live pods.
	for _, storedUID := range np.podConfigStore.ListPods() {
		if ns, isLive := livePodNetNs[storedUID]; isLive {
			np.podConfigStore.SetPodNetNs(storedUID, ns)
		}
	}

	return nil, nil
}

// CreateContainer handles container creation requests.
func (np *NetworkDriver) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid, "container", ctr.Name)
	ctx = klog.NewContext(ctx, logger)
	logger.V(2).Info("CreateContainer")
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
		logger.V(3).Info("Updating activity timestamp after CreateContainer")
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
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid)
	ctx = klog.NewContext(ctx, logger)
	logger.V(2).Info("RunPodSandbox")
	start := time.Now()
	status := statusNoop
	defer func() {
		nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, status).Inc()
		logger.V(2).Info("RunPodSandbox finished", "duration", time.Since(start))
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
func (np *NetworkDriver) runPodSandbox(ctx context.Context, pod *api.PodSandbox, podConfig PodConfig) error {
	logger := klog.FromContext(ctx)
	// get the pod network namespace
	ns := getNetworkNamespace(pod)
	// host network pods can not allocate network devices because it impact the host
	if ns == "" {
		return fmt.Errorf("RunPodSandbox pod %s/%s using host network can not claim host devices", pod.Namespace, pod.Name)
	}
	// store the Pod network namespace in the pod config store
	np.podConfigStore.SetPodNetNs(types.UID(pod.GetUid()), ns)

	// Track all the status updates needed for the resource claims of the pod.
	statusUpdates := map[types.NamespacedName]*resourceapply.ResourceClaimStatusApplyConfiguration{}
	// Process the configurations of the ResourceClaim
	for deviceName, config := range podConfig.DeviceConfigs {
		logger.V(4).Info("RunPodSandbox processing device", "device", deviceName, "config", fmt.Sprintf("%#v", config))
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
			if err := attachNetdevToNS(ctx, ns, deviceName, config, resourceClaimStatusDevice); err != nil {
				np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceAttachFailed",
					"failed to attach network device %s to pod %s/%s: %v", deviceName, pod.GetNamespace(), pod.GetName(), err)
				return err
			}
		}

		// Block 2: RDMA link device — independent of whether a netdev exists.
		// For IB-only devices (no netdev) this is the only operation here;
		// for RoCE (netdev + RDMA) it runs after the netdev block above.
		if !np.rdmaSharedMode && config.RDMADevice.LinkDev != "" {
			if err := attachRdmaToNS(ctx, config.RDMADevice.LinkDev, ns, resourceClaimStatusDevice); err != nil {
				np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "RDMADeviceAttachFailed",
					"failed to attach RDMA device %s to pod %s/%s: %v", config.RDMADevice.LinkDev, pod.GetNamespace(), pod.GetName(), err)
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
		claimLogger := klog.LoggerWithValues(logger, "claim", klog.KRef(claim.Namespace, claim.Name))
		go func() {
			ctxStatus, cancel := context.WithTimeout(klog.NewContext(context.Background(), claimLogger), 3*time.Second)
			defer cancel()
			_, err := np.kubeClient.ResourceV1().ResourceClaims(claim.Namespace).ApplyStatus(ctxStatus,
				resourceClaimApply,
				metav1.ApplyOptions{FieldManager: np.driverName, Force: true},
			)
			if err != nil {
				claimLogger.Info("Failed to update status for claim", "err", err)
			} else {
				claimLogger.V(4).Info("Updated status for claim")
			}
		}()
	}

	return nil
}

// attachRdmaToNS moves the RDMA link device into the pod network namespace and
// records the RDMALinkReady status condition on resourceClaimStatusDevice.
func attachRdmaToNS(ctx context.Context, linkDev, ns string, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "rdmaDevice", linkDev, "netns", ns)
	logger.V(2).Info("RunPodSandbox processing RDMA device")
	if err := nsAttachRdmadev(linkDev, ns); err != nil {
		logger.Info("RunPodSandbox error moving RDMA device to namespace", "err", err)
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
func attachNetdevToNS(ctx context.Context, ns, deviceName string, config DeviceConfig, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) error {
	ifName := config.NetworkInterfaceConfigInHost.Interface.Name
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "device", deviceName, "interface", ifName, "netns", ns)
	logger.V(2).Info("RunPodSandbox processing Network device")
	// TODO config options to rename the device and pass parameters
	// use https://github.com/opencontainers/runtime-spec/pull/1271
	networkData, err := nsAttachNetdev(ifName, ns, config.NetworkInterfaceConfigInPod.Interface)
	if err != nil {
		logger.Info("RunPodSandbox error moving network device to namespace", "err", err)
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
			logger.Info("RunPodSandbox error applying ethtool config", "podInterface", ifNameInNs, "err", err)
			return fmt.Errorf("error applying ethtool config for %s in ns %s: %v", ifNameInNs, ns, err)
		}
	}

	// Check if the ebpf programs should be disabled
	if config.NetworkInterfaceConfigInPod.Interface.DisableEBPFPrograms != nil &&
		*config.NetworkInterfaceConfigInPod.Interface.DisableEBPFPrograms {
		err := detachEBPFPrograms(ns, ifNameInNs)
		if err != nil {
			logger.Info("Error disabling ebpf programs", "podInterface", ifNameInNs, "err", err)
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
		logger.Info("RunPodSandbox error configuring routing", "podInterface", ifNameInNs, "err", err)
		return fmt.Errorf("error configuring device %s routes on namespace %s: %v", deviceName, ns, err)
	}

	// Configure rules
	// If VRF is enabled, rules are not needed/supported as routing is handled by the VRF table + l3mdev.
	if vrfTable == 0 {
		err = applyRulesConfig(ns, config.NetworkInterfaceConfigInPod.Rules)
		if err != nil {
			logger.Info("RunPodSandbox error configuring rules", "err", err)
			return fmt.Errorf("error configuring device %s rules on namespace %s: %v", deviceName, ns, err)
		}
	}

	// Configure neighbors
	err = applyNeighborConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Neighbors)
	if err != nil {
		logger.Info("RunPodSandbox failed to apply neighbor configuration", "podInterface", ifNameInNs, "err", err)
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
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid)
	ctx = klog.NewContext(ctx, logger)
	logger.V(2).Info("StopPodSandbox")
	start := time.Now()
	status := statusNoop
	defer func() {
		nriPluginRequestsTotal.WithLabelValues(methodStopPodSandbox, status).Inc()
		logger.V(2).Info("StopPodSandbox finished", "duration", time.Since(start))
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

func (np *NetworkDriver) stopPodSandbox(ctx context.Context, pod *api.PodSandbox, podConfig PodConfig) error {
	logger := klog.FromContext(ctx)
	// get the pod network namespace
	ns := getNetworkNamespace(pod)
	if ns == "" {
		// some version of containerd does not send the network namespace information on this hook so
		// we workaround it using the local copy we have in the db to associate interfaces with Pods via
		// the network namespace id.
		if podConfig.NetNS == "" {
			logger.Info("StopPodSandbox: network namespace for DRANET pod is unknown; skipping explicit device detach and relying on kernel netns teardown")
			return nil
		}
		ns = podConfig.NetNS
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
				logger.Error(err, "Failed to return rdma device", "device", deviceName)
			} else {
				rdmaDetached = true
			}
		}

		netdevDetached := false
		ifName := config.NetworkInterfaceConfigInPod.Interface.Name
		if ifName != "" {
			if err := nsDetachNetdev(ns, ifName, config.NetworkInterfaceConfigInHost.Interface.Name); err != nil {
				logger.Error(err, "Failed to return network device", "device", deviceName)
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
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid)
	ctx = klog.NewContext(ctx, logger)
	logger.V(2).Info("RemovePodSandbox")
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
	return nil
}

func (np *NetworkDriver) Shutdown(ctx context.Context) {
	klog.FromContext(ctx).Info("Runtime shutting down...")
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

// NRI gives us *api.PodSandbox while we need *v1.Pod for the Eventf.
// As such, we construct the minimal *v1.Pod object reference needed for the event.
func podObjectRef(pod *api.PodSandbox) *v1.Pod {
	p := &v1.Pod{}
	p.Name = pod.GetName()
	p.Namespace = pod.GetNamespace()
	p.UID = types.UID(pod.GetUid())
	return p
}
