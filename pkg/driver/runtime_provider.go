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
	"sort"
	"time"

	"github.com/containerd/nri/pkg/api"
	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/runtimeprovider"
)

// WithRuntimeProvider makes the driver have each provider configure each Pod
// with DRANET devices before the Pod's first container starts, in the order
// given. socket is the UNIX socket the OCI hook reaches the daemon through; it
// must be on a path the container runtime sees. timeout is how long the
// runtime lets the hook run.
func WithRuntimeProvider(socket string, timeout time.Duration, providers ...cloudprovider.RuntimeProvider) Option {
	return func(o *NetworkDriver) {
		o.runtimeProvider = runtimeprovider.New(socket, timeout, providers...)
	}
}

func (np *NetworkDriver) startRuntimeProvider(ctx context.Context) error {
	if np.runtimeProvider == nil {
		return nil
	}
	np.bindRuntimeProviderPersistence()
	return np.runtimeProvider.Start(ctx, np.handleRuntimeHook)
}

func (np *NetworkDriver) bindRuntimeProviderPersistence() {
	if np.runtimeProvider == nil || np.podConfigStore == nil {
		return
	}
	np.runtimeProvider.SetPersistence(
		func(podUID types.UID) bool {
			podCfg, ok := np.podConfigStore.GetPodConfig(podUID)
			return ok && podCfg.RuntimeProviderDone
		},
		np.podConfigStore.SetRuntimeProviderDone,
	)
}

func (np *NetworkDriver) resetRuntimeProvider(podUID types.UID) {
	if np.runtimeProvider != nil {
		np.runtimeProvider.Reset(podUID)
	}
}

func (np *NetworkDriver) runtimeProviderAdjustment(pod *api.PodSandbox, ctr *api.Container, podConfig PodConfig) (*api.Hook, error) {
	return np.runtimeProvider.Adjustment(types.UID(pod.GetUid()), ctr.GetId(), func() (*cloudprovider.ConfigurePodRequest, error) {
		netns := getNetworkNamespace(pod)
		if netns == "" {
			netns = podConfig.NetNS
		}
		if netns == "" {
			return nil, fmt.Errorf("runtime provider: the network namespace of pod %s is unknown", podKey(pod))
		}
		return buildConfigurePodRequest(pod, netns, podConfig), nil
	})
}

// buildConfigurePodRequest describes the Pod for the provider: its claims, as
// prepared, with their devices as configured.
func buildConfigurePodRequest(pod *api.PodSandbox, netns string, podConfig PodConfig) *cloudprovider.ConfigurePodRequest {
	byClaim := map[types.NamespacedName]*cloudprovider.PodClaim{}
	for name, config := range podConfig.DeviceConfigs {
		claim, ok := byClaim[config.Claim]
		if !ok {
			claim = &cloudprovider.PodClaim{Claim: config.ResourceClaim}
			if claim.Claim == nil {
				claim.Claim = &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: config.Claim.Namespace, Name: config.Claim.Name}}
			}
			byClaim[config.Claim] = claim
		}
		device := cloudprovider.PodDevice{
			Name:       name,
			Device:     cloudprovider.DeviceIdentifiersFromDevice(config.DeviceSnapshot),
			RDMADevice: config.RDMADevice.LinkDev,
		}
		if device.Device.Name == "" {
			device.Device.Name = config.NetworkInterfaceConfigInHost.Interface.Name
		}
		if config.NetworkInterfaceConfigInHost.Interface.Name != "" {
			networkConfig := config.NetworkInterfaceConfigInPod
			device.Config = &networkConfig
		}
		claim.Devices = append(claim.Devices, device)
	}
	claims := make([]cloudprovider.PodClaim, 0, len(byClaim))
	for _, claim := range byClaim {
		sort.Slice(claim.Devices, func(i, j int) bool { return claim.Devices[i].Name < claim.Devices[j].Name })
		claims = append(claims, *claim)
	}
	sort.Slice(claims, func(i, j int) bool {
		return claims[i].Claim.Namespace+"/"+claims[i].Claim.Name < claims[j].Claim.Namespace+"/"+claims[j].Claim.Name
	})
	return &cloudprovider.ConfigurePodRequest{
		Pod:    cloudprovider.PodReference{Namespace: pod.GetNamespace(), Name: pod.GetName(), UID: types.UID(pod.GetUid())},
		NetNS:  netns,
		Claims: claims,
	}
}

// handleRuntimeHook serves one hook process: it runs each configured runtime
// provider for the container's Pod and reports the outcome, which decides
// whether the container starts.
func (np *NetworkDriver) handleRuntimeHook(ctx context.Context, containerID string) error {
	start := time.Now()
	pod, outcome, err := np.runtimeProvider.Handle(ctx, containerID)
	status := statusFailed
	switch outcome {
	case runtimeprovider.OutcomeSuccess:
		status = statusSuccess
	case runtimeprovider.OutcomeNoop:
		status = statusNoop
	}
	runtimeProviderRequestsTotal.WithLabelValues(status).Inc()
	runtimeProviderLatencySeconds.WithLabelValues(status).Observe(time.Since(start).Seconds())
	if err != nil {
		if pod.UID != "" && np.eventRecorder != nil {
			np.eventRecorder.Eventf(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID}},
				v1.EventTypeWarning, "RuntimeProviderFailed", "%v", err)
		}
		return err
	}
	return nil
}
