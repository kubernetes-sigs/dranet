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

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const (
	PathGetDeviceAttributes  = "/GetDeviceAttributes"
	PathGetDeviceConfig      = "/GetDeviceConfig"
	PathGetProfileConfig     = "/GetProfileConfig"
	PathReleaseProfileConfig = "/ReleaseProfileConfig"
	PathHealth               = "/health"
)

// Capabilities represents the functionality supported by the webhook server.
type Capabilities struct {
	CloudProvider   bool `json:"cloudProvider"`
	ProfileProvider bool `json:"profileProvider"`
}

// API Contracts (JSON payloads)
type ProfileRequest struct {
	Device cloudprovider.DeviceIdentifiers `json:"device"`
	Claim  *resourceapi.ResourceClaim      `json:"claim"`
	Config *apis.NetworkConfig             `json:"config,omitempty"`
}

// ProfileReleaseRequest contains the stable identity needed to release a
// profile. NodeUnprepareResources does not provide the full ResourceClaim.
type ProfileReleaseRequest struct {
	Device   cloudprovider.DeviceIdentifiers `json:"device"`
	ClaimUID types.UID                       `json:"claim_uid"`
	Config   *apis.NetworkConfig             `json:"config,omitempty"`
}

// WebhookProvider implements CloudInstance and ProfileProvider via HTTP POST.
type WebhookProvider struct {
	baseURL *url.URL
	client  *http.Client
	caps    Capabilities
}

func (p *WebhookProvider) HasCloudProvider() bool {
	return p.caps.CloudProvider
}

func (p *WebhookProvider) HasProfileProvider() bool {
	return p.caps.ProfileProvider
}

func newWebhookClient(endpoint string) (*http.Client, *url.URL, error) {
	if endpoint == "" {
		return nil, nil, fmt.Errorf("webhook endpoint is empty")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid webhook URL: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if u.Scheme == "unix" {
		socketPath := u.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		}
		u, _ = url.Parse("http://localhost")
	}

	client := &http.Client{
		Transport: transport,
	}
	return client, u, nil
}

// OnWebhook tries to reach the webhook server to determine if it is available.
func OnWebhook(ctx context.Context, endpoint string) bool {
	client, u, err := newWebhookClient(endpoint)
	if err != nil {
		return false
	}
	client.Timeout = 5 * time.Second

	reqURL := u.JoinPath(PathHealth).String()

	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 5*time.Second, true, func(ctx context.Context) (done bool, err error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return false, nil
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false, nil
		}
		return true, nil
	})

	return err == nil
}

var _ cloudprovider.CloudInstance = &WebhookProvider{}
var _ cloudprovider.ProfileProvider = &WebhookProvider{}

// NewWebhookProvider initializes the HTTP client and fetches capabilities.
func NewWebhookProvider(ctx context.Context, endpoint string) (*WebhookProvider, error) {
	client, u, err := newWebhookClient(endpoint)
	if err != nil {
		return nil, err
	}
	client.Timeout = 10 * time.Second

	p := &WebhookProvider{
		baseURL: u,
		client:  client,
	}

	reqURL := p.baseURL.JoinPath(PathHealth).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create health request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to reach webhook health endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("webhook health endpoint returned status %d", resp.StatusCode)
	}

	var caps Capabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		return nil, fmt.Errorf("webhook capabilities decoding failed: %w", err)
	}
	p.caps = caps

	return p, nil
}

// helper to perform the HTTP POST
func (p *WebhookProvider) post(path string, reqPayload interface{}, respObj interface{}) error {
	if (path == PathGetDeviceAttributes || path == PathGetDeviceConfig) && !p.caps.CloudProvider {
		return fmt.Errorf("webhook returned HTTP %d: Not Implemented", http.StatusNotImplemented)
	}
	if (path == PathGetProfileConfig || path == PathReleaseProfileConfig) && !p.caps.ProfileProvider {
		return fmt.Errorf("webhook returned HTTP %d: Not Implemented", http.StatusNotImplemented)
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %v", err)
	}

	reqURL := p.baseURL.JoinPath(path).String()
	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webhook returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if respObj != nil {
		if err := json.NewDecoder(resp.Body).Decode(respObj); err != nil {
			return fmt.Errorf("failed to decode webhook response: %v", err)
		}
	}
	return nil
}

// GetDeviceAttributes asks the webhook for physical hardware attributes.
func (p *WebhookProvider) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	var resp map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	err := p.post(PathGetDeviceAttributes, id, &resp)
	if err != nil {
		klog.Errorf("Webhook GetDeviceAttributes failed: %v", err)
		return nil
	}

	return resp
}

// GetDeviceConfig asks the webhook for baseline physical network settings (like MTU).
func (p *WebhookProvider) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	var config apis.NetworkConfig
	err := p.post(PathGetDeviceConfig, id, &config)
	if err != nil {
		klog.Errorf("Webhook GetDeviceConfig failed: %v", err)
		return nil
	}
	return &config
}

// GetProfileConfig asks the webhook to resolve the logical profile (e.g., allocate an IP).
func (p *WebhookProvider) GetProfileConfig(id cloudprovider.DeviceIdentifiers, claim *resourceapi.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	req := ProfileRequest{
		Device: id,
		Claim:  claim,
		Config: config,
	}

	var respConfig apis.NetworkConfig
	err := p.post(PathGetProfileConfig, req, &respConfig)
	if err != nil {
		return nil, err
	}
	return &respConfig, nil
}

// ReleaseProfileConfig tells the webhook to free stateful resources (e.g., release IP).
func (p *WebhookProvider) ReleaseProfileConfig(id cloudprovider.DeviceIdentifiers, claimUID types.UID, config *apis.NetworkConfig) error {
	req := ProfileReleaseRequest{
		Device:   id,
		ClaimUID: claimUID,
		Config:   config,
	}

	return p.post(PathReleaseProfileConfig, req, nil)
}
