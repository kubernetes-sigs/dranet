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

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/dranet/pkg/cloudprovider/coreweave"
	"sigs.k8s.io/dranet/pkg/cloudprovider/discovery"
	"sigs.k8s.io/dranet/pkg/cloudprovider/webhook"
)

// TestSetupProviders tests the initialization behavior of the dranet providers.
// We avoid testing actual cloud providers (like GCE, AWS, Azure, OKE) here because
// their discovery functions poll real metadata servers. Running these tests on a VM
// in one of those clouds would generate false positives or unpredictable behavior.
// Instead, we use the webhook provider to inject our own local mock server, allowing
// us to assert the business logic consistently.
func TestSetupProviders(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name              string
		cloudProviderHint string
		profileProvider   string
		webhookURL        string // explicit URL to bypass mock server creation
		webhookCaps       *webhook.Capabilities
		expectCloudInst   bool
		expectProfProv    bool
		expectErr         bool
	}{
		{
			name:              "No providers",
			cloudProviderHint: "NONE",
			profileProvider:   "none",
			expectCloudInst:   false,
			expectProfProv:    false,
			expectErr:         false,
		},
		{
			name:              "Cloud provider webhook init failure does not hard fail",
			cloudProviderHint: "webhook",
			profileProvider:   "none",
			webhookURL:        "://invalid-url",
			expectCloudInst:   false, // Returns nil because init fails
			expectProfProv:    false,
			expectErr:         false, // But doesn't hard fail
		},
		{
			name:              "Profile provider webhook empty URL hard fails",
			cloudProviderHint: "NONE",
			profileProvider:   "webhook",
			webhookURL:        "empty", // special case to mean ""
			expectCloudInst:   false,
			expectProfProv:    false,
			expectErr:         true,
		},
		{
			name:              "Profile provider webhook init failure hard fails",
			cloudProviderHint: "NONE",
			profileProvider:   "webhook",
			webhookURL:        "://invalid-url",
			expectCloudInst:   false,
			expectProfProv:    false,
			expectErr:         true,
		},
		{
			name:              "Both webhook providers succeed and reuse instance",
			cloudProviderHint: "webhook",
			profileProvider:   "webhook",
			webhookCaps:       &webhook.Capabilities{CloudProvider: true, ProfileProvider: true},
			expectCloudInst:   true,
			expectProfProv:    true,
			expectErr:         false,
		},
		{
			name:              "Profile provider cloud reuses cloud instance",
			cloudProviderHint: "webhook",
			profileProvider:   "cloud",
			webhookCaps:       &webhook.Capabilities{CloudProvider: true, ProfileProvider: true},
			expectCloudInst:   true,
			expectProfProv:    true,
			expectErr:         false,
		},
		{
			name:              "Profile provider cloud with nil cloud instance results in nil profProv",
			cloudProviderHint: "NONE",
			profileProvider:   "cloud",
			webhookURL:        "empty",
			expectCloudInst:   false,
			expectProfProv:    false,
			expectErr:         false, // profProv gracefully becomes nil
		},
		{
			name:              "CloudProvider capability missing falls back to nil",
			cloudProviderHint: "webhook",
			profileProvider:   "none",
			webhookCaps:       &webhook.Capabilities{CloudProvider: false, ProfileProvider: true},
			expectCloudInst:   false,
			expectProfProv:    false,
			expectErr:         false, // cloud provider missing capability degrades to nil gracefully
		},
		{
			name:              "ProfileProvider capability missing fails hard",
			cloudProviderHint: "NONE",
			profileProvider:   "webhook",
			webhookCaps:       &webhook.Capabilities{CloudProvider: true, ProfileProvider: false},
			expectCloudInst:   false,
			expectProfProv:    false,
			expectErr:         true, // profile provider missing capability is a hard failure
		},
		{
			name:              "Both missing capabilities degrades cloudInst but fails profProv",
			cloudProviderHint: "webhook",
			profileProvider:   "webhook",
			webhookCaps:       &webhook.Capabilities{CloudProvider: false, ProfileProvider: false},
			expectCloudInst:   false, // degraded
			expectProfProv:    false,
			expectErr:         true, // profProv fails
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := tt.webhookURL
			if endpoint == "empty" {
				endpoint = ""
			} else if tt.webhookCaps != nil {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == webhook.PathHealth {
						json.NewEncoder(w).Encode(tt.webhookCaps)
						return
					}
					w.WriteHeader(http.StatusOK)
				}))
				defer srv.Close()
				endpoint = srv.URL
			}

			cloudInst, profProv, err := setupProviders(ctx, providerOptions{
				cloudProviderHint: tt.cloudProviderHint,
				profileProvider:   tt.profileProvider,
				webhookURL:        endpoint,
			})

			if (err != nil) != tt.expectErr {
				t.Errorf("expected error: %v, got: %v", tt.expectErr, err)
			}

			if (cloudInst != nil) != tt.expectCloudInst {
				t.Errorf("expected cloudInst: %v, got: %v", tt.expectCloudInst, cloudInst != nil)
			}

			if (profProv != nil) != tt.expectProfProv {
				t.Errorf("expected profProv: %v, got: %v", tt.expectProfProv, profProv != nil)
			}
		})
	}
}

func TestSetupProvidersCKS(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "cks-node",
		Labels: map[string]string{
			coreweave.LabelCKSCluster:   "use15",
			coreweave.LabelFabricFlavor: "infiniband",
		},
	}}
	dependencies := discovery.Dependencies{
		NodeClient: fake.NewSimpleClientset(node).CoreV1().Nodes(),
		NodeName:   node.Name,
	}

	cloudInstance, profileProvider, err := setupProviders(context.Background(), providerOptions{
		cloudProviderHint: "CKS",
		profileProvider:   "cloud",
		dependencies:      dependencies,
	})
	if err != nil {
		t.Fatalf("setupProviders() error = %v", err)
	}
	if _, ok := cloudInstance.(*coreweave.Instance); !ok {
		t.Fatalf("setupProviders() cloud instance = %T, want *coreweave.Instance", cloudInstance)
	}
	if profileProvider != nil {
		t.Fatalf("setupProviders() profile provider = %T, want nil", profileProvider)
	}
}
