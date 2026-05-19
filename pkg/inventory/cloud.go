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

package inventory

import (
	"context"
	"strings"

	"cloud.google.com/go/compute/metadata"

	"k8s.io/klog/v2"

	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/cloudprovider/alibaba"
	"sigs.k8s.io/dranet/pkg/cloudprovider/aws"
	"sigs.k8s.io/dranet/pkg/cloudprovider/azure"
	"sigs.k8s.io/dranet/pkg/cloudprovider/gce"
	"sigs.k8s.io/dranet/pkg/cloudprovider/oke"
)

type CloudProviderHint string

const (
	CloudProviderHintGCE     CloudProviderHint = "GCE"
	CloudProviderHintAWS     CloudProviderHint = "AWS"
	CloudProviderHintAzure   CloudProviderHint = "AZURE"
	CloudProviderHintOKE     CloudProviderHint = "OKE"
	CloudProviderHintAlibaba CloudProviderHint = "ALIBABA"
	CloudProviderHintNone    CloudProviderHint = "NONE"
)

func discoverCloudProvider(ctx context.Context) CloudProviderHint {
	discoverers := map[CloudProviderHint]func(context.Context) bool{
		CloudProviderHintGCE: func(ctx context.Context) bool {
			return metadata.OnGCE()
		},
		CloudProviderHintAWS: aws.OnAWS,
		CloudProviderHintAzure:   azure.OnAzure,
		CloudProviderHintOKE:     oke.OnOKE,
		CloudProviderHintAlibaba: alibaba.OnAlibaba,
	}

	for hint, discoverer := range discoverers {
		if discoverer(ctx) {
			return hint
		}
	}

	klog.Warning("could not discover cloud provider")
	return CloudProviderHintNone
}

// getInstanceProperties gets the instance properties using the cloud provider hint if provided.
func getInstanceProperties(ctx context.Context, cloudProviderHint CloudProviderHint) cloudprovider.CloudInstance {
	if cloudProviderHint == CloudProviderHintNone {
		klog.Infof("cloud provider hint is none, skipping instance properties retrieval")
		return nil
	}

	providers := map[CloudProviderHint]func(context.Context) (cloudprovider.CloudInstance, error){
		CloudProviderHintGCE:     gce.GetInstance,
		CloudProviderHintAWS:     aws.GetInstance,
		CloudProviderHintAzure:   azure.GetInstance,
		CloudProviderHintOKE:     oke.GetInstance,
		CloudProviderHintAlibaba: alibaba.GetInstance,
	}

	provider, ok := providers[cloudProviderHint]
	if !ok {
		klog.Infof("unknown cloud provider hint: %s", cloudProviderHint)
		return nil
	}
	instance, err := provider(ctx)
	if err != nil {
		klog.Infof("could not get instance properties: %v", err)
		return nil
	}
	return instance
}

// getProviderAttributes retrieves cloud provider-specific attributes for a network interface
func getProviderAttributes(device *resourceapi.Device, instance cloudprovider.CloudInstance) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	if instance == nil {
		klog.Warningf("instance metadata is nil, cannot get provider attributes.")
		return nil
	}

	if device == nil {
		klog.Warningf("device is nil, cannot get provider attributes.")
		return nil
	}

	id := cloudprovider.DeviceIdentifiers{
		Name: device.Name,
	}
	// get the device identifiers from the device attributes
	if macAttr, ok := device.Attributes[apis.AttrMac]; ok && macAttr.StringValue != nil {
		id.MAC = *macAttr.StringValue
	}
	if pciAttr, ok := device.Attributes[apis.AttrPCIAddress]; ok && pciAttr.StringValue != nil {
		id.PCIAddress = *pciAttr.StringValue
	}

	return instance.GetDeviceAttributes(id)
}

// getLastSegmentAndTruncate extracts the last segment from a path
// and truncates it to the specified maximum length.
func getLastSegmentAndTruncate(s string, maxLength int) string {
	segments := strings.Split(s, "/")
	if len(segments) == 0 {
		// This condition is technically unreachable because strings.Split always returns a slice with at least one element.
		// For an empty input string, segments will be []string{""}.
		return ""
	}
	lastSegment := segments[len(segments)-1]
	if len(lastSegment) > maxLength {
		return lastSegment[:maxLength]
	}
	return lastSegment
}