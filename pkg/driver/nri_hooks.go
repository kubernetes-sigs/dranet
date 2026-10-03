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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"time"

	"github.com/containerd/nri/pkg/api"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	metav1apply "k8s.io/client-go/applyconfigurations/meta/v1"
	resourceapply "k8s.io/client-go/applyconfigurations/resource/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/set"
	"sigs.k8s.io/dranet/pkg/apis"
	hookapi "sigs.k8s.io/dranet/pkg/apis/hook"
)

// NRI hooks into the container runtime, the lifecycle of the Pod seen here is local to the runtime
// and is not the same as the Pod lifecycle for kubernetes, per example, a Pod that can fail to start
// is retried locally multiple times, so the hooks need to be idempotent to all operations on the Pod.
// The NRI hooks are time sensitive, any slow operation needs to be added on the DRA hooks and only
// the information necessary should passed to the NRI hooks via the np.podConfigStore so it can be executed
// quickly. Attaching devices is the exception; see attach_job.go.

// errNRIBudgetExceeded is returned when attaching the Pod's devices does not fit
// in the request and deferring is disabled. The runtime treats a plugin error
// returned before its deadline as a failure of the operation; a reply after the
// deadline disconnects the plugin and the operation proceeds without the devices.
var errNRIBudgetExceeded = errors.New("not enough time left in the NRI request to attach all the network devices; increase the container runtime NRI plugin_request_timeout")

func (np *NetworkDriver) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	logger := klog.FromContext(ctx)
	logger.Info("Synchronized state with the runtime", "pods", len(pods), "containers", len(containers))

	// livePods tracks live pods by UID.
	livePods := make(map[types.UID]*api.PodSandbox)
	for _, pod := range pods {
		podLogger := klog.LoggerWithValues(logger, "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid)
		podLogger.Info("Synchronize Pod")
		podLogger.V(2).Info("Pod network details", "netns", getNetworkNamespace(pod), "ips", pod.GetIps())
		livePods[types.UID(pod.Uid)] = pod
	}

	// Process stored pods: update NetNS for live pods.
	for _, storedUID := range np.podConfigStore.ListPods() {
		if pod, isLive := livePods[storedUID]; isLive {
			np.podConfigStore.SetPodNetNs(storedUID, getNetworkNamespace(pod))
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

func (np *NetworkDriver) createContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container, podConfig PodConfig) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	logger := klog.FromContext(ctx)
	adjust := &api.ContainerAdjustment{}

	// The container must not start until every device of the Pod is attached.
	if pending, _ := np.pendingDevices(podConfig); pending > 0 {
		barrier, err := np.gateContainerOnAttach(ctx, pod)
		if err != nil {
			return nil, nil, err
		}
		if barrier != nil {
			containerHooksTotal.WithLabelValues(hookTypeBarrier).Inc()
			// The attach may have progressed while waiting.
			podConfig, _ = np.podConfigStore.GetPodConfig(types.UID(pod.GetUid()))
			env, err := np.hookEnv(pod, ctr, podConfig)
			if err != nil {
				return nil, nil, err
			}
			barrier.Env = env
			adjust.AddHooks(&api.Hooks{CreateRuntime: []*api.Hook{barrier}})
			logger.V(2).Info("Added the createRuntime hook to the container", "hookTimeout", barrier.Timeout.GetValue())
		}
	}

	// Containers only care about the RDMA char devices.
	devPaths := set.Set[string]{}

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

// gateContainerOnAttach makes sure the container does not start before all
// the Pod's devices are attached. It waits for the attach job within the
// request budget; if the job is still running it returns the barrier hook to
// add to the container, which blocks on the hook socket until the job ends.
// A failed job is retried until the Pod's attach deadline.
func (np *NetworkDriver) gateContainerOnAttach(ctx context.Context, pod *api.PodSandbox) (*api.Hook, error) {
	logger := klog.FromContext(ctx)
	job, err := np.attachJobForContainer(ctx, pod)
	if err != nil || job == nil {
		return nil, err
	}

	// Often the job ends within this request's budget and no hook is needed.
	waitCtx, cancel := requestBudget(ctx)
	defer cancel()
	if job.wait(waitCtx) {
		return nil, job.err
	}

	podConfig, _ := np.podConfigStore.GetPodConfig(types.UID(pod.GetUid()))
	pending, total := np.pendingDevices(podConfig)
	if np.hookPath == "" {
		return nil, fmt.Errorf("network devices of pod %s/%s: %d/%d attached, the kubelet will retry: %w", pod.GetNamespace(), pod.GetName(), total-pending, total, errNRIBudgetExceeded)
	}
	// The runtime kills the hook at its timeout and fails the container, so
	// the hook gets the Pod's remaining attach time, rounded up.
	hookTimeout := max(int(math.Ceil(time.Until(job.deadline).Seconds())), 1)
	logger.V(2).Info("Injected the device attach hook into the container", "pending", pending, "total", total, "hookTimeout", hookTimeout)
	return &api.Hook{
		Path:    np.hookPath,
		Args:    []string{filepath.Base(np.hookPath), "wait"},
		Timeout: api.Int(hookTimeout),
	}, nil
}

// hookEnv is the environment of the createRuntime hook of a container: what
// DRANET decided for the Pod (package apis/hook), so the hook does not look
// the Pod or its devices up elsewhere while they change.
func (np *NetworkDriver) hookEnv(pod *api.PodSandbox, ctr *api.Container, podConfig PodConfig) ([]string, error) {
	devices := make([]apis.HookDevice, 0, len(podConfig.DeviceConfigs))
	for name, config := range podConfig.DeviceConfigs {
		device := apis.HookDevice{
			Name:        name,
			Claim:       apis.HookClaimRef{Namespace: config.Claim.Namespace, Name: config.Claim.Name},
			Host:        apis.DeviceIdentifiersFromDevice(config.DeviceSnapshot),
			Interface:   config.NetworkInterfaceConfigInPod.Interface.Name,
			RDMALinkDev: config.RDMADevice.LinkDev,
		}
		if device.Host.Name == "" {
			device.Host.Name = config.NetworkInterfaceConfigInHost.Interface.Name
		}
		if np.needsAttach(config) {
			conf := config.NetworkInterfaceConfigInPod
			device.Config = &conf
		}
		devices = append(devices, device)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	devicesJSON, err := json.Marshal(devices)
	if err != nil {
		return nil, fmt.Errorf("encode the hook devices: %w", err)
	}
	ns := getNetworkNamespace(pod)
	if ns == "" {
		ns = podConfig.NetNS
	}
	return []string{
		hookapi.EnvPodUID + "=" + pod.GetUid(),
		hookapi.EnvPodNamespace + "=" + pod.GetNamespace(),
		hookapi.EnvPodName + "=" + pod.GetName(),
		hookapi.EnvContainerName + "=" + ctr.GetName(),
		hookapi.EnvNetNS + "=" + ns,
		hookapi.EnvSocket + "=" + hookapi.SocketPath,
		hookapi.EnvDevices + "=" + string(devicesJSON),
	}, nil
}

// attachJobForContainer returns the job a container must wait on, or nil when
// the Pod's devices are all attached. It starts a job when the previous one
// failed, or when none exists because the driver restarted since
// RunPodSandbox, and fails once the Pod's attach deadline has passed.
func (np *NetworkDriver) attachJobForContainer(ctx context.Context, pod *api.PodSandbox) (*attachJob, error) {
	logger := klog.FromContext(ctx)
	podUID := types.UID(pod.GetUid())
	np.attachJobsMu.Lock()
	defer np.attachJobsMu.Unlock()

	podConfig, ok := np.podConfigStore.GetPodConfig(podUID)
	if !ok {
		return nil, nil
	}
	pending, total := np.pendingDevices(podConfig)
	if pending == 0 {
		return nil, nil
	}
	job := np.attachJobs[podUID]
	if job != nil && !job.finished() {
		return job, nil
	}

	now := time.Now()
	var deadline time.Time
	if job != nil {
		deadline = job.deadline
	}
	if deadline.IsZero() {
		if np.deviceAttachTimeout == 0 {
			return nil, fmt.Errorf("pod %s/%s has %d/%d network devices pending and deferred attach is disabled (--device-attach-timeout=0)", pod.GetNamespace(), pod.GetName(), pending, total)
		}
		// No job: the driver restarted since RunPodSandbox.
		deadline = now.Add(np.deviceAttachTimeout)
	}
	if !now.Before(deadline) {
		attachDeadlineExceededTotal.Inc()
		err := fmt.Errorf("gave up attaching network devices to pod %s/%s after %s: %d/%d attached. The sandbox stays and the kubelet keeps retrying this error; delete the pod to start over, and increase the container runtime NRI plugin_request_timeout", pod.GetNamespace(), pod.GetName(), np.deviceAttachTimeout, total-pending, total)
		logger.Error(err, "CreateContainer giving up on pending devices")
		np.eventRecorder.Event(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceAttachFailed", err.Error())
		return nil, err
	}

	ns := getNetworkNamespace(pod)
	if ns == "" {
		ns = podConfig.NetNS
	}
	if ns == "" {
		return nil, fmt.Errorf("network namespace of pod %s/%s is unknown, can not attach its %d/%d pending network devices", pod.GetNamespace(), pod.GetName(), pending, total)
	}
	if job != nil {
		logger.Info("Retrying the network device attach", "err", job.err, "attached", total-pending, "total", total)
	}
	return np.startAttachJob(pod, podConfig, ns, deadline), nil
}

// needsAttach reports whether the device has something to move into the Pod's
// network namespace: a netdev, or an RDMA link in exclusive RDMA netns mode.
// Devices that only inject RDMA char devices are handled by createContainer.
func (np *NetworkDriver) needsAttach(config DeviceConfig) bool {
	return config.NetworkInterfaceConfigInHost.Interface.Name != "" ||
		(!np.rdmaSharedMode && config.RDMADevice.LinkDev != "")
}

// pendingDevices returns how many devices of the Pod still need to be attached
// and how many need attaching at all.
func (np *NetworkDriver) pendingDevices(podConfig PodConfig) (pending, total int) {
	for _, config := range podConfig.DeviceConfigs {
		if !np.needsAttach(config) {
			continue
		}
		total++
		if !config.Attached {
			pending++
		}
	}
	return pending, total
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
	podUID := types.UID(pod.GetUid())
	if deadline, ok := ctx.Deadline(); ok {
		nriRequestTimeoutSeconds.Set(time.Until(deadline).Round(100 * time.Millisecond).Seconds())
	}
	// get the pod network namespace
	ns := getNetworkNamespace(pod)
	// host network pods can not allocate network devices because it impact the host
	if ns == "" {
		return fmt.Errorf("RunPodSandbox pod %s/%s using host network can not claim host devices", pod.Namespace, pod.Name)
	}
	job, err := np.startSandboxAttach(ctx, pod, ns)
	if err != nil || job == nil {
		return err
	}
	waitCtx, cancel := requestBudget(ctx)
	defer cancel()
	if job.wait(waitCtx) {
		// An attach error fails the sandbox: the runtime destroys the network
		// namespace and the kernel returns the devices to the host.
		if job.err != nil {
			sandboxAttachTotal.WithLabelValues(attachResultFailed).Inc()
		} else {
			sandboxAttachTotal.WithLabelValues(attachResultAttached).Inc()
		}
		return job.err
	}

	current, _ := np.podConfigStore.GetPodConfig(podUID)
	pending, total := np.pendingDevices(current)
	if np.deviceAttachTimeout == 0 {
		job.cancel()
		sandboxAttachTotal.WithLabelValues(attachResultRejected).Inc()
		np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceAttachTimeout",
			"attached %d/%d network devices to pod %s/%s within the container runtime NRI request timeout; failing the sandbox so the pod does not start with missing devices. Increase the container runtime NRI plugin_request_timeout", total-pending, total, pod.GetNamespace(), pod.GetName())
		return fmt.Errorf("attached %d/%d network devices: %w", total-pending, total, errNRIBudgetExceeded)
	}
	sandboxAttachTotal.WithLabelValues(attachResultDeferred).Inc()
	np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceAttachDeferred",
		"attached %d/%d network devices to pod %s/%s within the container runtime NRI request timeout; the attach continues and the pod's containers wait for it, up to %s. Increase the container runtime NRI plugin_request_timeout to attach all devices at sandbox creation", total-pending, total, pod.GetNamespace(), pod.GetName(), np.deviceAttachTimeout)
	logger.Info("RunPodSandbox returning with the network device attach in progress", "attached", total-pending, "total", total, "deadline", job.deadline)
	return nil
}

// startSandboxAttach records the new sandbox's network namespace, forgets the
// attach progress of any previous sandbox of the Pod, and starts the attach
// job; nil when the Pod has no device to attach.
func (np *NetworkDriver) startSandboxAttach(ctx context.Context, pod *api.PodSandbox, ns string) (*attachJob, error) {
	podUID := types.UID(pod.GetUid())
	np.podConfigStore.SetPodNetNs(podUID, ns)
	// A job of a previous sandbox of the Pod must not touch the new namespace.
	np.stopAttachJob(ctx, podUID)
	podConfig, err := np.podConfigStore.ResetAttachProgress(podUID)
	if err != nil {
		return nil, err
	}
	if _, total := np.pendingDevices(podConfig); total == 0 {
		return nil, nil
	}
	var deadline time.Time
	if np.deviceAttachTimeout > 0 {
		deadline = time.Now().Add(np.deviceAttachTimeout)
	}
	np.attachJobsMu.Lock()
	defer np.attachJobsMu.Unlock()
	return np.startAttachJob(pod, podConfig, ns, deadline), nil
}

// attachDevices attaches the Pod's pending devices to its network namespace ns,
// one at a time, until ctx is done. It returns how many devices are still
// pending and the total. A device that fails to attach returns an error; the
// devices attached before it stay attached and recorded.
func (np *NetworkDriver) attachDevices(ctx context.Context, pod *api.PodSandbox, podConfig PodConfig, ns string) (pending, total int, err error) {
	logger := klog.FromContext(ctx)
	podUID := types.UID(pod.GetUid())
	_, total = np.pendingDevices(podConfig)

	for deviceName, config := range podConfig.DeviceConfigs {
		if !np.needsAttach(config) || config.Attached {
			continue
		}
		if ctx.Err() != nil {
			pending++
			continue
		}
		logger.V(4).Info("Processing device", "device", deviceName, "config", fmt.Sprintf("%#v", config))
		deviceStart := time.Now()
		resourceClaim := types.NamespacedName{Name: config.Claim.Name, Namespace: config.Claim.Namespace}
		// resourceClaim status for this specific device, applied once it is attached
		resourceClaimStatusDevice := resourceapply.
			AllocatedDeviceStatus().
			WithDevice(deviceName).
			WithDriver(np.driverName).
			WithPool(np.nodeName)

		ifName := config.NetworkInterfaceConfigInHost.Interface.Name

		// Block 1: netdev operations — only when a network interface is present.
		if ifName != "" {
			if config.NetworkInterfaceConfigInPod.Interface.IsSubinterface() {
				if err := createSubinterfaceInNS(ctx, ns, deviceName, config, resourceClaimStatusDevice); err != nil {
					deviceAttachDurationSeconds.WithLabelValues(attachResultFailed).Observe(time.Since(deviceStart).Seconds())
					np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceCreateFailed",
						"failed to create subinterface on network device %s to pod %s/%s: %v", deviceName, pod.GetNamespace(), pod.GetName(), err)
					return 0, total, err
				}
			} else if err := attachNetdevToNS(ctx, ns, deviceName, config, resourceClaimStatusDevice); err != nil {
				deviceAttachDurationSeconds.WithLabelValues(attachResultFailed).Observe(time.Since(deviceStart).Seconds())
				np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceAttachFailed",
					"failed to attach network device %s to pod %s/%s: %v", deviceName, pod.GetNamespace(), pod.GetName(), err)
				return 0, total, err
			}
		}

		// Block 2: RDMA link device — independent of whether a netdev exists.
		// For IB-only devices (no netdev) this is the only operation here;
		// for RoCE (netdev + RDMA) it runs after the netdev block above.
		if !np.rdmaSharedMode && config.RDMADevice.LinkDev != "" {
			if err := attachRdmaToNS(ctx, config.RDMADevice.LinkDev, ns, resourceClaimStatusDevice); err != nil {
				deviceAttachDurationSeconds.WithLabelValues(attachResultFailed).Observe(time.Since(deviceStart).Seconds())
				np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "RDMADeviceAttachFailed",
					"failed to attach RDMA device %s to pod %s/%s: %v", config.RDMADevice.LinkDev, pod.GetNamespace(), pod.GetName(), err)
				return 0, total, err
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

		np.applyDeviceStatus(ctx, resourceClaim, deviceName, resourceClaimStatusDevice)
		deviceAttachDurationSeconds.WithLabelValues(attachResultAttached).Observe(time.Since(deviceStart).Seconds())
		// The kernel state changed; record it even if the checkpoint fails so
		// StopPodSandbox and the hooks see the device as attached.
		if err := np.podConfigStore.SetDeviceAttached(podUID, deviceName, true); err != nil {
			logger.Error(err, "Failed to record device as attached", "device", deviceName)
		}
	}
	return pending, total, nil
}

// applyDeviceStatus writes one device's status to its ResourceClaim in the
// background, so the NRI handler is not blocked by the API server. Each device
// is applied under its own field manager: status.devices is a keyed list and a
// server-side apply removes the entries its manager owned but no longer sends,
// so devices attached in different hook calls must not share a manager.
func (np *NetworkDriver) applyDeviceStatus(ctx context.Context, claim types.NamespacedName, deviceName string, device *resourceapply.AllocatedDeviceStatusApplyConfiguration) {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "claim", klog.KRef(claim.Namespace, claim.Name), "device", deviceName)
	resourceClaimApply := resourceapply.ResourceClaim(claim.Name, claim.Namespace).
		WithStatus(resourceapply.ResourceClaimStatus().WithDevices(device))
	go func() {
		ctxStatus, cancel := context.WithTimeout(klog.NewContext(context.Background(), logger), 3*time.Second)
		defer cancel()
		_, err := np.kubeClient.ResourceV1().ResourceClaims(claim.Namespace).ApplyStatus(ctxStatus,
			resourceClaimApply,
			metav1.ApplyOptions{FieldManager: np.driverName + "/" + deviceName, Force: true},
		)
		if err != nil {
			logger.Error(err, "Failed to update status for claim")
		} else {
			logger.V(4).Info("Updated status for claim")
		}
	}()
}

// attachRdmaToNS moves the RDMA link device into the pod network namespace and
// records the RDMALinkReady status condition on resourceClaimStatusDevice.
func attachRdmaToNS(ctx context.Context, linkDev, ns string, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "rdmaDevice", linkDev, "netns", ns)
	logger.V(2).Info("RunPodSandbox processing RDMA device")
	if err := nsAttachRdmadev(linkDev, ns); err != nil {
		logger.Error(err, "RunPodSandbox error moving RDMA device to namespace")
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
	networkData, err := attachNetdev(ifName, ns, config.NetworkInterfaceConfigInPod.Interface)
	if err != nil {
		logger.Error(err, "RunPodSandbox error moving network device to namespace")
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

	// Configure the moved device (ethtool, vrf, routes, neighbors, rules)
	return configureNetdevInNS(ctx, ns, deviceName, config, networkData.InterfaceName, resourceClaimStatusDevice)
}

// createSubinterfaceInNS creates a subinterface in the pod network namespace,
// applies all associated configurations, and records the status conditions.
func createSubinterfaceInNS(ctx context.Context, ns, deviceName string, config DeviceConfig, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) error {
	logger := klog.FromContext(ctx)
	hostIfName := config.NetworkInterfaceConfigInHost.Interface.Name
	logger.V(2).Info("RunPodSandbox creating subinterface on parent device", "parentDevice", hostIfName)

	networkData, err := nsCreateSubinterface(hostIfName, ns, config.NetworkInterfaceConfigInPod.Interface)
	if err != nil {
		logger.Error(err, "RunPodSandbox error creating subinterface", "parentDevice", hostIfName, "netns", ns)
		return fmt.Errorf("error creating subinterface on parent %s in namespace %s: %v", hostIfName, ns, err)
	}

	// Configure the subinterface (ethtool, vrf, routes, neighbors, rules)
	if err := configureNetdevInNS(ctx, ns, deviceName, config, networkData.InterfaceName, resourceClaimStatusDevice); err != nil {
		// Delete the child now rather than leaving it half configured until pod teardown.
		if delErr := nsDeleteSubinterface(ns, networkData.InterfaceName); delErr != nil {
			return errors.Join(err, fmt.Errorf("failed to delete subinterface %s after a configuration failure: %w", networkData.InterfaceName, delErr))
		}
		return err
	}

	// Report the device only after the configuration succeeds, so a failure
	// leaves the status without a Ready condition or network data.
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
	)
	return nil
}

// configureNetdevInNS applies common L3 configurations (ethtool, eBPF, VRF, routes, rules, and neighbors)
// to a network interface inside the container's network namespace and marks the claim status as NetworkReady.
func configureNetdevInNS(ctx context.Context, ns, deviceName string, config DeviceConfig, ifNameInNs string, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) error {
	logger := klog.FromContext(ctx)
	var err error

	// Apply Ethtool configurations
	if config.NetworkInterfaceConfigInPod.Ethtool != nil {
		err = applyEthtoolConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Ethtool)
		if err != nil {
			logger.Error(err, "RunPodSandbox error applying ethtool config", "podInterface", ifNameInNs)
			return fmt.Errorf("error applying ethtool config for %s in ns %s: %v", ifNameInNs, ns, err)
		}
	}

	// Check if the ebpf programs should be disabled
	if config.NetworkInterfaceConfigInPod.Interface.DisableEBPFPrograms != nil &&
		*config.NetworkInterfaceConfigInPod.Interface.DisableEBPFPrograms {
		err = detachEBPFPrograms(ns, ifNameInNs)
		if err != nil {
			logger.Error(err, "Error disabling ebpf programs", "podInterface", ifNameInNs)
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
		logger.Error(err, "RunPodSandbox error configuring routing", "podInterface", ifNameInNs)
		return fmt.Errorf("error configuring device %s routes on namespace %s: %v", deviceName, ns, err)
	}

	// Configure rules
	// If VRF is enabled, rules are not needed/supported as routing is handled by the VRF table + l3mdev.
	if vrfTable == 0 {
		err = applyRulesConfig(ns, config.NetworkInterfaceConfigInPod.Rules)
		if err != nil {
			logger.Error(err, "RunPodSandbox error configuring rules")
			return fmt.Errorf("error configuring device %s rules on namespace %s: %v", deviceName, ns, err)
		}
	}

	// Configure neighbors
	err = applyNeighborConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Neighbors)
	if err != nil {
		logger.Error(err, "RunPodSandbox failed to apply neighbor configuration", "podInterface", ifNameInNs)
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
	podUID := types.UID(pod.GetUid())
	// Nothing may move devices into the namespace while they are detached.
	np.stopAttachJob(ctx, podUID)
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
			if config.NetworkInterfaceConfigInPod.Interface.IsSubinterface() {
				subIfName := config.NetworkInterfaceConfigInPod.Interface.Name
				if err := nsDeleteSubinterface(ns, subIfName); err != nil {
					logger.Error(err, "Failed to delete subinterface", "subInterface", subIfName, "device", deviceName)
				}
			} else {
				if err := nsDetachNetdev(ns, ifName, config.NetworkInterfaceConfigInHost.Interface.Name); err != nil {
					logger.Error(err, "Failed to return network device", "device", deviceName)
				} else {
					netdevDetached = true
				}
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

func (np *NetworkDriver) removePodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	podUID := types.UID(pod.GetUid())
	np.stopAttachJob(ctx, podUID)
	np.deleteAttachJob(podUID)
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
