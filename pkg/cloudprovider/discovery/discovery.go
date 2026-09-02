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
	NodeClient corev1client.NodeInterface
	NodeName   string
}

type cloudProviderProbe struct {
	hint  CloudProviderHint
	match func() bool
}

// DiscoverCloudProvider probes the environment using additional Kubernetes-local
// provider inputs when available to detect which cloud provider DRANET is running on.
func DiscoverCloudProvider(ctx context.Context, webhookURL string, dependencies Dependencies) CloudProviderHint {
	return detectCloudProvider(cloudProviderProbes(ctx, webhookURL, dependencies))
}

func cloudProviderProbes(ctx context.Context, webhookURL string, dependencies Dependencies) []cloudProviderProbe {
	return []cloudProviderProbe{
		{hint: CloudProviderHintGCE, match: metadata.OnGCE},
		{hint: CloudProviderHintAWS, match: func() bool { return aws.OnAWS(ctx) }},
		{hint: CloudProviderHintAzure, match: func() bool { return azure.OnAzure(ctx) }},
		{hint: CloudProviderHintOKE, match: func() bool { return oke.OnOKE(ctx) }},
		{hint: CloudProviderHintAlibaba, match: func() bool { return alibaba.OnAlibaba(ctx) }},
		{hint: CloudProviderHintCKS, match: func() bool {
			return coreweave.OnCKS(ctx, dependencies.NodeClient, dependencies.NodeName)
		}},
		{hint: CloudProviderHintWebhook, match: func() bool {
			return webhookURL != "" && webhook.OnWebhook(ctx, webhookURL)
		}},
	}
}

func detectCloudProvider(probes []cloudProviderProbe) CloudProviderHint {
	for _, probe := range probes {
		if probe.match() {
			return probe.hint
		}
	}
	return CloudProviderHintNone
}

// GetInstanceProperties initializes the specified cloud provider using additional
// Kubernetes-local provider inputs when available.
func GetInstanceProperties(ctx context.Context, hint CloudProviderHint, webhookURL string, dependencies Dependencies) (cloudprovider.CloudInstance, error) {
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
		return coreweave.GetInstance(ctx, dependencies.NodeClient, dependencies.NodeName)
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
