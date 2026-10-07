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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/nri/pkg/api"

	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
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
	// What moving devices has cost this request so far; see attachBudget.
	budget := &attachBudget{}
	// Subinterfaces with addressing "SLAAC", and their claims, waited for
	// together once every device is in place; see pendingSubinterface.
	var pendingSubinterfaces []*pendingSubinterface
	var pendingClaims []types.NamespacedName
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
		statusDeferred := false

		// Block 1: netdev operations — only when a network interface is present.
		if ifName != "" {
			if config.NetworkInterfaceConfigInPod.Interface.IsSubinterface() {
				p, err := createSubinterfaceInNS(ctx, ns, deviceName, config, resourceClaimStatusDevice)
				if err != nil {
					np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceCreateFailed",
						"failed to create subinterface on network device %s to pod %s/%s: %v", deviceName, pod.GetNamespace(), pod.GetName(), err)
					return err
				}
				if p != nil {
					// Its address is waited for after the loop, together with
					// the other subinterfaces, and its status added then.
					pendingSubinterfaces = append(pendingSubinterfaces, p)
					pendingClaims = append(pendingClaims, resourceClaim)
					statusDeferred = true
				}
			} else if err := attachNetdevToNS(ctx, ns, deviceName, config, resourceClaimStatusDevice, np.slaacReadyTimeout, np.slaacRollbackReserve, budget); err != nil {
				var notReady *slaacNotReadyError
				switch {
				case errors.As(err, &notReady) && notReady.outcome == slaacLeftInPod:
					// The runtime starts the Pod regardless: keep the interface,
					// record why it has no address, and attach the rest.
					np.recordSLAACLeftInPod(pod, notReady, resourceClaimStatusDevice)
				case notReady != nil:
					np.reportSLAACNotReady(logger, pod, []slaacFailure{{claim: resourceClaim, notReady: notReady}})
					return err
				default:
					np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceAttachFailed",
						"failed to attach network device %s to pod %s/%s: %v", deviceName, pod.GetNamespace(), pod.GetName(), err)
					return err
				}
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

		if !statusDeferred {
			resourceClaimStatus.WithDevices(resourceClaimStatusDevice)
		}
	}

	if len(pendingSubinterfaces) > 0 {
		results, pastDeadline, err := awaitSubinterfacesSLAAC(ctx, ns, pendingSubinterfaces, np.slaacReadyTimeout, np.slaacRollbackReserve)
		if results == nil {
			return err
		}
		outcome := slaacDeleted
		switch {
		case pastDeadline:
			outcome = slaacLeftInPod
		case err != nil:
			outcome = slaacCleanupFailed
		}
		var failures []slaacFailure
		for i, p := range pendingSubinterfaces {
			if results[i].err == nil {
				continue
			}
			notReady := &slaacNotReadyError{deviceName: p.deviceName, ifName: p.networkData.InterfaceName, err: results[i].err, outcome: outcome}
			if pastDeadline {
				// The runtime starts the Pod regardless: keep the child,
				// record why it has no address, and complete the others.
				np.recordSLAACLeftInPod(pod, notReady, p.status)
				statusUpdates[pendingClaims[i]].WithDevices(p.status)
				continue
			}
			failures = append(failures, slaacFailure{claim: pendingClaims[i], notReady: notReady})
		}
		if len(failures) > 0 || err != nil {
			np.reportSLAACNotReady(logger, pod, failures)
			errs := []error{err}
			for _, f := range failures {
				errs = append(errs, f.notReady)
			}
			return errors.Join(errs...)
		}
		for i, p := range pendingSubinterfaces {
			if results[i].err != nil {
				continue
			}
			if err := finishSubinterfaceInNS(ctx, ns, p, results[i].addresses); err != nil {
				np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceCreateFailed",
					"failed to configure subinterface on network device %s to pod %s/%s: %v", p.deviceName, pod.GetNamespace(), pod.GetName(), err)
				return err
			}
			statusUpdates[pendingClaims[i]].WithDevices(p.status)
		}
	}

	// do not block the handler to update the status
	for claim, status := range statusUpdates {
		np.applyClaimStatus(logger, claim, status)
	}

	return nil
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

// slaacOutcome is what became of an interface whose IPv6 autoconfiguration
// did not complete.
type slaacOutcome int

const (
	// slaacReturnedToHost: a moved interface went back to the host.
	slaacReturnedToHost slaacOutcome = iota
	// slaacDeleted: the subinterface was deleted with the others of the Pod.
	slaacDeleted
	// slaacLeftInPod: the runtime request deadline had passed, so the
	// interface stays in the Pod, which the runtime starts regardless.
	slaacLeftInPod
	// slaacCleanupFailed: taking the interface back out of the Pod failed.
	slaacCleanupFailed
)

// slaacNotReadyError reports an interface that did not finish IPv6
// autoconfiguration within its budget, and what became of it.
type slaacNotReadyError struct {
	deviceName string
	ifName     string
	err        error
	outcome    slaacOutcome
}

// slaacOutcomeOf tells from the error of a passthrough wait what became of
// the interface.
func slaacOutcomeOf(err error) slaacOutcome {
	switch {
	case errors.Is(err, errSLAACLeftInPod):
		return slaacLeftInPod
	case errors.Is(err, errSLAACRollbackFailed):
		return slaacCleanupFailed
	default:
		return slaacReturnedToHost
	}
}

// describe says what became of the interface and of the sandbox, for the event.
func (e *slaacNotReadyError) describe() string {
	switch e.outcome {
	case slaacDeleted:
		return "the subinterfaces of the Pod were deleted and the sandbox fails so the kubelet retries"
	case slaacLeftInPod:
		return "the runtime request deadline had passed, so the interface was left in the Pod without an autoconfigured address"
	case slaacCleanupFailed:
		return "removing the interface from the Pod failed and the sandbox fails so the kubelet retries"
	default:
		return "the interface was returned to the host and the sandbox fails so the kubelet retries"
	}
}

// slaacNotReadyCondition is the SLAACReady=False condition recorded on the
// claim for an interface that did not complete autoconfiguration.
func slaacNotReadyCondition(notReady *slaacNotReadyError) *metav1apply.ConditionApplyConfiguration {
	return metav1apply.Condition().
		WithType("SLAACReady").
		WithStatus(metav1.ConditionFalse).
		WithReason("AutoconfigurationTimedOut").
		WithMessage(notReady.err.Error()).
		WithLastTransitionTime(metav1.Now())
}

// slaacFailure is an interface whose autoconfiguration failed, with its claim.
type slaacFailure struct {
	claim    types.NamespacedName
	notReady *slaacNotReadyError
}

// reportSLAACNotReady records interfaces that did not finish IPv6
// autoconfiguration, for a sandbox that fails because of them, on the Pod's
// events and on their claims: the events are the first place to look and the
// claim is the second, and nothing else is left behind to explain a Pod that
// never starts. The claim status is applied once per claim with all of its
// failed devices, since each apply replaces the devices the previous one
// listed.
func (np *NetworkDriver) reportSLAACNotReady(logger klog.Logger, pod *api.PodSandbox, failures []slaacFailure) {
	byClaim := map[types.NamespacedName]*resourceapply.ResourceClaimStatusApplyConfiguration{}
	for _, f := range failures {
		np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceNotReady",
			"%v in pod %s/%s; %s", f.notReady, pod.GetNamespace(), pod.GetName(), f.notReady.describe())
		status := byClaim[f.claim]
		if status == nil {
			status = resourceapply.ResourceClaimStatus()
			byClaim[f.claim] = status
		}
		status.WithDevices(resourceapply.AllocatedDeviceStatus().
			WithDevice(f.notReady.deviceName).
			WithDriver(np.driverName).
			WithPool(np.nodeName).
			WithConditions(slaacNotReadyCondition(f.notReady)))
	}
	for claim, status := range byClaim {
		np.applyClaimStatus(logger, claim, status)
	}
}

// recordSLAACLeftInPod records an interface that was left in the Pod without
// an address after the request deadline: an event, and the SLAACReady=False
// condition on the device status the claim update carries with the others.
func (np *NetworkDriver) recordSLAACLeftInPod(pod *api.PodSandbox, notReady *slaacNotReadyError, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) {
	np.eventRecorder.Eventf(podObjectRef(pod), v1.EventTypeWarning, "NetworkDeviceNotReady",
		"%v in pod %s/%s; %s", notReady, pod.GetNamespace(), pod.GetName(), notReady.describe())
	resourceClaimStatusDevice.WithConditions(slaacNotReadyCondition(notReady)).
		WithNetworkData(resourceapply.NetworkDeviceData().WithInterfaceName(notReady.ifName))
}

func (e *slaacNotReadyError) Error() string {
	return fmt.Sprintf("interface %s of network device %s did not complete IPv6 autoconfiguration: %v", e.ifName, e.deviceName, e.err)
}

func (e *slaacNotReadyError) Unwrap() error { return e.err }

// applyClaimStatus updates a claim's status in the background, so the runtime
// hook that produced it does not wait on the API server.
func (np *NetworkDriver) applyClaimStatus(logger klog.Logger, claim types.NamespacedName, status *resourceapply.ResourceClaimStatusApplyConfiguration) {
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
			claimLogger.Error(err, "Failed to update status for claim")
		} else {
			claimLogger.V(4).Info("Updated status for claim")
		}
	}()
}

// attachNetdevToNS moves the host network interface into the pod network namespace,
// applies all associated configuration (ethtool, eBPF, routes, rules, neighbors),
// waits for IPv6 autoconfiguration when the interface uses it, and records the
// resulting status conditions on resourceClaimStatusDevice. budget is shared by
// the devices of one request; see attachBudget.
func attachNetdevToNS(ctx context.Context, ns, deviceName string, config DeviceConfig, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration, slaacReadyTimeout, slaacRollbackReserve time.Duration, budget *attachBudget) error {
	ifName := config.NetworkInterfaceConfigInHost.Interface.Name
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "device", deviceName, "interface", ifName, "netns", ns)
	logger.V(2).Info("RunPodSandbox processing Network device")
	slaac := config.NetworkInterfaceConfigInPod.Interface.Addressing == apis.AddressingModeSLAAC

	// A SLAAC interface may have to come back if its address never arrives,
	// and that costs about what the move does. Only move it while both still
	// fit in the request; failing here leaves the host untouched and the
	// sandbox retried, where a late reply would leave the Pod without it.
	if slaac {
		if err := budget.check(ctx, ifName, slaacRollbackReserve); err != nil {
			logger.Error(err, "RunPodSandbox not moving the network device")
			return err
		}
	}

	// TODO config options to rename the device and pass parameters
	// use https://github.com/opencontainers/runtime-spec/pull/1271
	start := time.Now()
	networkData, err := nsAttachNetdev(ifName, ns, config.NetworkInterfaceConfigInPod.Interface)
	if err != nil {
		logger.Error(err, "RunPodSandbox error moving network device to namespace")
		return fmt.Errorf("error moving network device %s to namespace %s: %v", deviceName, ns, err)
	}
	attachTook := time.Since(start)
	budget.record(attachTook)

	// Link-level configuration first (ethtool, eBPF, VRF): enslaving the
	// interface to a VRF cycles the link and drops the address it had, so a
	// SLAAC wait has to come after it.
	vrfTable, err := configureLinkInNS(ctx, ns, deviceName, config, networkData.InterfaceName)
	if err != nil {
		return err
	}

	// With SLAAC the interface is up but not yet usable: its address arrives
	// from a router advertisement some milliseconds later. Wait for it here,
	// while there is still a namespace to roll back out of, and before the
	// routes: one through a gateway in the advertised prefix is unreachable
	// until the advertisement has arrived. Rolling back costs about what the
	// move did, so that is the least the wait leaves for it.
	if slaac {
		reserve := max(slaacRollbackReserve, attachTook)
		addresses, err := awaitSLAACReady(ctx, ns, networkData.InterfaceName, ifName, slaacReadyTimeout, reserve)
		if err != nil {
			logger.Error(err, "RunPodSandbox interface did not complete IPv6 autoconfiguration")
			// Past the request deadline the interface stays in the Pod without
			// an address: its routes are left out, and the caller records it
			// and goes on with the other devices.
			return &slaacNotReadyError{deviceName: deviceName, ifName: networkData.InterfaceName, err: err, outcome: slaacOutcomeOf(err)}
		}
		networkData.IPs = append(networkData.IPs, addresses...)
		resourceClaimStatusDevice.WithConditions(
			metav1apply.Condition().
				WithType("SLAACReady").
				WithStatus(metav1.ConditionTrue).
				WithReason("SLAACReady").
				WithMessage(fmt.Sprintf("autoconfigured addresses: %s", strings.Join(addresses, ","))).
				WithLastTransitionTime(metav1.Now()),
		)
	}

	// Routes, rules and neighbors, now that the addresses they may depend on
	// are in place.
	if err := configureRoutingInNS(ctx, ns, deviceName, config, networkData.InterfaceName, vrfTable, resourceClaimStatusDevice); err != nil {
		return err
	}

	// Report the device only now, so a failure above leaves the status
	// without a Ready condition or network data.
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
	return nil
}

// createSubinterfaceInNS creates a subinterface in the pod network namespace,
// applies all associated configurations, and records the status conditions.
//
// A subinterface with addressing "SLAAC" only gets its link-level configuration
// here and is returned as pending: its address is waited for together with the
// other subinterfaces of the Pod, and finishSubinterfaceInNS completes it.
func createSubinterfaceInNS(ctx context.Context, ns, deviceName string, config DeviceConfig, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) (*pendingSubinterface, error) {
	logger := klog.FromContext(ctx)
	hostIfName := config.NetworkInterfaceConfigInHost.Interface.Name
	logger.V(2).Info("RunPodSandbox creating subinterface on parent device", "parentDevice", hostIfName)

	networkData, err := nsCreateSubinterface(hostIfName, ns, config.NetworkInterfaceConfigInPod.Interface)
	if err != nil {
		logger.Error(err, "RunPodSandbox error creating subinterface", "parentDevice", hostIfName, "netns", ns)
		return nil, fmt.Errorf("error creating subinterface on parent %s in namespace %s: %v", hostIfName, ns, err)
	}
	// Delete the child now rather than leaving it half configured until pod teardown.
	deleteOnError := func(err error) error {
		if delErr := nsDeleteSubinterface(ns, networkData.InterfaceName); delErr != nil {
			return errors.Join(err, fmt.Errorf("failed to delete subinterface %s after a configuration failure: %w", networkData.InterfaceName, delErr))
		}
		return err
	}

	if config.NetworkInterfaceConfigInPod.Interface.Addressing == apis.AddressingModeSLAAC {
		// Link-level configuration first: enslaving the child to a VRF cycles
		// the link and drops the address it had.
		vrfTable, err := configureLinkInNS(ctx, ns, deviceName, config, networkData.InterfaceName)
		if err != nil {
			return nil, deleteOnError(err)
		}
		return &pendingSubinterface{deviceName: deviceName, config: config, vrfTable: vrfTable, networkData: networkData, status: resourceClaimStatusDevice}, nil
	}

	// Configure the subinterface (ethtool, vrf, routes, neighbors, rules)
	if err := configureNetdevInNS(ctx, ns, deviceName, config, networkData.InterfaceName, resourceClaimStatusDevice); err != nil {
		return nil, deleteOnError(err)
	}
	reportSubinterfaceReady(resourceClaimStatusDevice, networkData)
	return nil, nil
}

// finishSubinterfaceInNS completes a pending subinterface once its
// autoconfigured addresses are in place: it records them, applies the routes,
// rules and neighbors that may depend on them, and reports the device ready.
func finishSubinterfaceInNS(ctx context.Context, ns string, p *pendingSubinterface, addresses []string) error {
	p.networkData.IPs = append(p.networkData.IPs, addresses...)
	p.status.WithConditions(
		metav1apply.Condition().
			WithType("SLAACReady").
			WithStatus(metav1.ConditionTrue).
			WithReason("SLAACReady").
			WithMessage(fmt.Sprintf("autoconfigured addresses: %s", strings.Join(addresses, ","))).
			WithLastTransitionTime(metav1.Now()),
	)
	if err := configureRoutingInNS(ctx, ns, p.deviceName, p.config, p.networkData.InterfaceName, p.vrfTable, p.status); err != nil {
		if delErr := nsDeleteSubinterface(ns, p.networkData.InterfaceName); delErr != nil {
			return errors.Join(err, fmt.Errorf("failed to delete subinterface %s after a configuration failure: %w", p.networkData.InterfaceName, delErr))
		}
		return err
	}
	reportSubinterfaceReady(p.status, p.networkData)
	return nil
}

// reportSubinterfaceReady records the Ready condition and the network data of
// a subinterface. It runs only after the configuration succeeds, so a failure
// leaves the status without a Ready condition or network data.
func reportSubinterfaceReady(resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration, networkData *resourceapi.NetworkDeviceData) {
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
}

// configureNetdevInNS applies common L3 configurations (ethtool, eBPF, VRF, routes, rules, and neighbors)
// to a network interface inside the container's network namespace and marks the claim status as NetworkReady.
func configureNetdevInNS(ctx context.Context, ns, deviceName string, config DeviceConfig, ifNameInNs string, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) error {
	vrfTable, err := configureLinkInNS(ctx, ns, deviceName, config, ifNameInNs)
	if err != nil {
		return err
	}
	return configureRoutingInNS(ctx, ns, deviceName, config, ifNameInNs, vrfTable, resourceClaimStatusDevice)
}

// configureLinkInNS applies the link-level configuration (ethtool, eBPF, VRF)
// and returns the VRF table the routing has to use, 0 for none. It is the
// half that has to happen before an autoconfigured address is waited for:
// enslaving the interface to a VRF cycles the link and drops the address.
func configureLinkInNS(ctx context.Context, ns, deviceName string, config DeviceConfig, ifNameInNs string) (int, error) {
	logger := klog.FromContext(ctx)
	var err error

	// Apply Ethtool configurations
	if config.NetworkInterfaceConfigInPod.Ethtool != nil {
		err = applyEthtoolConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Ethtool)
		if err != nil {
			logger.Error(err, "RunPodSandbox error applying ethtool config", "podInterface", ifNameInNs)
			return 0, fmt.Errorf("error applying ethtool config for %s in ns %s: %v", ifNameInNs, ns, err)
		}
	}

	// Check if the ebpf programs should be disabled
	if config.NetworkInterfaceConfigInPod.Interface.DisableEBPFPrograms != nil &&
		*config.NetworkInterfaceConfigInPod.Interface.DisableEBPFPrograms {
		err = detachEBPFPrograms(ns, ifNameInNs)
		if err != nil {
			logger.Error(err, "Error disabling ebpf programs", "podInterface", ifNameInNs)
			return 0, fmt.Errorf("error disabling ebpf programs for %s in ns %s: %v", ifNameInNs, ns, err)
		}
	}

	vrfTable := 0
	if config.NetworkInterfaceConfigInPod.Interface.VRF != nil {
		vrfTable, err = applyVRFConfig(ns, ifNameInNs, config.NetworkInterfaceConfigInPod.Interface.VRF)
		if err != nil {
			return 0, fmt.Errorf("error configuring VRF for device %s in ns %s: %w", deviceName, ns, err)
		}
	}
	return vrfTable, nil
}

// configureRoutingInNS applies routes, rules and neighbors and marks the claim
// status as NetworkReady. It is the half that has to happen after an
// autoconfigured address is in place: a route through a gateway in the
// advertised prefix is unreachable until the advertisement has arrived.
func configureRoutingInNS(ctx context.Context, ns, deviceName string, config DeviceConfig, ifNameInNs string, vrfTable int, resourceClaimStatusDevice *resourceapply.AllocatedDeviceStatusApplyConfiguration) error {
	logger := klog.FromContext(ctx)
	var err error

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
				hostIfName := config.NetworkInterfaceConfigInHost.Interface.Name
				if err := nsDetachNetdev(ns, ifName, hostIfName); err != nil {
					if _, hostErr := nlwrap.LinkByName(hostIfName); hostErr == nil {
						// Already on the host: a SLAAC rollback returned it before
						// the sandbox failed, or it was never attached.
						logger.V(2).Info("Network device is already on the host", "device", deviceName, "interface", hostIfName)
					} else {
						logger.Error(err, "Failed to return network device", "device", deviceName)
					}
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
