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

package discovery

import (
	"context"
	"fmt"

	"cloud.google.com/go/compute/metadata"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/cloudprovider/alibaba"
	"sigs.k8s.io/dranet/pkg/cloudprovider/aws"
	"sigs.k8s.io/dranet/pkg/cloudprovider/azure"
	"sigs.k8s.io/dranet/pkg/cloudprovider/coreweave"
	"sigs.k8s.io/dranet/pkg/cloudprovider/gce"
	"sigs.k8s.io/dranet/pkg/cloudprovider/oke"
	"sigs.k8s.io/dranet/pkg/cloudprovider/webhook"
)

type CloudProviderHint string

const (
	CloudProviderHintGCE     CloudProviderHint = "GCE"
	CloudProviderHintAWS     CloudProviderHint = "AWS"
	CloudProviderHintAzure   CloudProviderHint = "AZURE"
	CloudProviderHintOKE     CloudProviderHint = "OKE"
	CloudProviderHintAlibaba CloudProviderHint = "ALIBABA"
	CloudProviderHintCKS     CloudProviderHint = "CKS"
	CloudProviderHintWebhook CloudProviderHint = "webhook"
	CloudProviderHintNone    CloudProviderHint = "NONE"
)

// Dependencies contains Kubernetes-local inputs used by providers that do not
// expose a conventional instance metadata service.
type Dependencies struct {
	Nodes    corev1client.NodeInterface
	NodeName string
}

// DiscoverCloudProvider probes the environment to detect which cloud provider DRANET is running on.
func DiscoverCloudProvider(ctx context.Context, webhookURL string) CloudProviderHint {
	return DiscoverCloudProviderWithDependencies(ctx, webhookURL, Dependencies{})
}

// DiscoverCloudProviderWithDependencies probes the environment using additional
// Kubernetes-local provider inputs when available.
func DiscoverCloudProviderWithDependencies(ctx context.Context, webhookURL string, dependencies Dependencies) CloudProviderHint {
	// CKS is checked first because its authoritative metadata is already in the
	// Kubernetes Node object. Avoid slower link-local metadata probes on CKS.
	if coreweave.OnCKS(ctx, dependencies.Nodes, dependencies.NodeName) {
		return CloudProviderHintCKS
	}
	if metadata.OnGCE() {
		return CloudProviderHintGCE
	}
	if aws.OnAWS(ctx) {
		return CloudProviderHintAWS
	}
	if azure.OnAzure(ctx) {
		return CloudProviderHintAzure
	}
	if oke.OnOKE(ctx) {
		return CloudProviderHintOKE
	}
	if alibaba.OnAlibaba(ctx) {
		return CloudProviderHintAlibaba
	}
	if webhookURL != "" && webhook.OnWebhook(ctx, webhookURL) {
		return CloudProviderHintWebhook
	}
	return CloudProviderHintNone
}

// GetInstanceProperties initializes and returns the specified cloud provider instance.
func GetInstanceProperties(ctx context.Context, hint CloudProviderHint, webhookURL string) (cloudprovider.CloudInstance, error) {
	return GetInstancePropertiesWithDependencies(ctx, hint, webhookURL, Dependencies{})
}

// GetInstancePropertiesWithDependencies initializes the specified cloud provider
// using additional Kubernetes-local provider inputs when available.
func GetInstancePropertiesWithDependencies(ctx context.Context, hint CloudProviderHint, webhookURL string, dependencies Dependencies) (cloudprovider.CloudInstance, error) {
	switch hint {
	case CloudProviderHintGCE:
		return gce.GetInstance(ctx)
	case CloudProviderHintAWS:
		return aws.GetInstance(ctx)
	case CloudProviderHintAzure:
		return azure.GetInstance(ctx)
	case CloudProviderHintOKE:
		return oke.GetInstance(ctx)
	case CloudProviderHintAlibaba:
		return alibaba.GetInstance(ctx)
	case CloudProviderHintCKS:
		return coreweave.GetInstance(ctx, dependencies.Nodes, dependencies.NodeName)
	case CloudProviderHintWebhook:
		if webhookURL == "" {
			return nil, fmt.Errorf("--webhook-url is required when using the webhook cloud provider")
		}
		p, err := webhook.NewWebhookProvider(ctx, webhookURL)
		if err != nil {
			return nil, err
		}
		if !p.HasCloudProvider() {
			return nil, nil
		}
		return p, nil
	case CloudProviderHintNone, "none", "":
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown cloud provider hint: %s", hint)
	}
}
