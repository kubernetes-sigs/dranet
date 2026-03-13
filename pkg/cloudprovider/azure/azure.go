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

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const (
	AzureAttrPrefix = "azure.dra.net"

	AttrAzurePlacementGroupID = AzureAttrPrefix + "/" + "placementGroupId"
	AttrAzureVMSize           = AzureAttrPrefix + "/" + "vmSize"

	// imdsEndpoint is the Azure Instance Metadata Service endpoint.
	imdsEndpoint = "http://169.254.169.254/metadata/instance"
	// imdsAPIVersion is the API version used for IMDS queries.
	imdsAPIVersion = "2021-02-01"
)

// imdsComputeMetadata contains the fields we care about from the Azure IMDS
// compute metadata response.
type imdsComputeMetadata struct {
	PlacementGroupID string `json:"placementGroupId"`
	VMSize           string `json:"vmSize"`
}

// imdsResponse represents the top-level IMDS response structure.
type imdsResponse struct {
	Compute imdsComputeMetadata `json:"compute"`
}

var _ cloudprovider.CloudInstance = (*AzureInstance)(nil)

// AzureInstance holds Azure-specific instance data retrieved from IMDS.
type AzureInstance struct {
	PlacementGroupID string
	VMSize           string
}

// GetDeviceAttributes returns Azure-specific attributes for a device.
// PlacementGroupID and VMSize are node-level properties that apply to all
// devices on the node.
func (a *AzureInstance) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)

	if a.VMSize != "" {
		attributes[AttrAzureVMSize] = resourceapi.DeviceAttribute{StringValue: &a.VMSize}
	}

	if a.PlacementGroupID != "" {
		attributes[AttrAzurePlacementGroupID] = resourceapi.DeviceAttribute{StringValue: &a.PlacementGroupID}
	}

	return attributes
}

// GetDeviceConfig returns nil as Azure does not currently provide
// device-specific network configuration via IMDS.
func (a *AzureInstance) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	return nil
}

// OnAzure returns true if the code is running on an Azure VM by probing the
// IMDS endpoint. This is a lightweight check that does not parse the full
// response.
func OnAzure() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest("GET", imdsEndpoint+"?api-version="+imdsAPIVersion+"&format=text", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Metadata", "true")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// IMDS returns 200 on Azure VMs. Any successful response indicates Azure.
	return resp.StatusCode == http.StatusOK
}

// GetInstance retrieves Azure instance properties by querying IMDS.
func GetInstance(ctx context.Context) (cloudprovider.CloudInstance, error) {
	var instance *AzureInstance

	client := &http.Client{Timeout: 5 * time.Second}

	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 15*time.Second, true, func(ctx context.Context) (done bool, err error) {
		url := fmt.Sprintf("%s/compute?api-version=%s&format=json", imdsEndpoint, imdsAPIVersion)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			klog.Infof("could not create Azure IMDS request ... retrying: %v", err)
			return false, nil
		}
		req.Header.Set("Metadata", "true")

		resp, err := client.Do(req)
		if err != nil {
			klog.Infof("could not query Azure IMDS ... retrying: %v", err)
			return false, nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			klog.Infof("Azure IMDS returned status %d ... retrying", resp.StatusCode)
			return false, nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			klog.Infof("could not read Azure IMDS response ... retrying: %v", err)
			return false, nil
		}

		var computeMetadata imdsComputeMetadata
		if err := json.Unmarshal(body, &computeMetadata); err != nil {
			klog.Infof("could not parse Azure IMDS compute metadata ... retrying: %v", err)
			return false, nil
		}

		instance = &AzureInstance{
			PlacementGroupID: computeMetadata.PlacementGroupID,
			VMSize:           computeMetadata.VMSize,
		}

		klog.Infof("Azure IMDS: vmSize=%s, placementGroupId=%s", instance.VMSize, instance.PlacementGroupID)
		return true, nil
	})

	if err != nil {
		return nil, err
	}
	return instance, nil
}
