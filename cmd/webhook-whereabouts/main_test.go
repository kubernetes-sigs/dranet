package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/cloudprovider/webhook"
)

func ifname(name string) string {
	return cniIfname(name)
}

func TestPodFromClaim(t *testing.T) {
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", UID: "claim-uid"},
		Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{{
			Resource: "pods",
			Name:     "workload",
			UID:      "pod-uid",
		}}},
	}

	pod, err := podFromClaim(claim)
	if err != nil {
		t.Fatalf("podFromClaim() error = %v", err)
	}
	if pod.Namespace != "tenant-a" || pod.Name != "workload" {
		t.Fatalf("podFromClaim() = %#v, want tenant-a/workload", pod)
	}
}

func TestPodFromClaimRejectsInvalidClaims(t *testing.T) {
	validConsumer := resourceapi.ResourceClaimConsumerReference{Resource: "pods", Name: "workload", UID: "pod-uid"}
	tests := []struct {
		name  string
		claim *resourceapi.ResourceClaim
	}{
		{name: "nil claim"},
		{name: "missing UID", claim: &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{validConsumer}}}},
		{name: "missing namespace", claim: &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: "claim-uid"}, Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{validConsumer}}}},
		{name: "missing consumer", claim: &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: "claim-uid"}}},
		{name: "multiple consumers", claim: &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: "claim-uid"}, Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{validConsumer, validConsumer}}}},
		{name: "non-Pod consumer", claim: &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: "claim-uid"}, Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{{Resource: "services", Name: "service", UID: "service-uid"}}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := podFromClaim(tt.claim); err == nil {
				t.Fatal("podFromClaim() accepted an invalid ResourceClaim")
			}
		})
	}
}

func TestCNIEnvUsesClaimAndPodIdentity(t *testing.T) {
	env := cniEnv("ADD", "claim-uid", "0000-27-00-2", podIdentity{Namespace: "tenant-a", Name: "workload"}, "/opt/cni/bin")
	want := []string{
		"CNI_COMMAND=ADD",
		"CNI_CONTAINERID=claim-uid",
		"CNI_NETNS=/dev/null",
		"CNI_IFNAME=0000-27-00-2",
		"CNI_PATH=/opt/cni/bin",
		"CNI_ARGS=IgnoreUnknown=1;K8S_POD_NAMESPACE=tenant-a;K8S_POD_NAME=workload;K8S_POD_INFRA_CONTAINER_ID=claim-uid",
	}
	if !slices.Equal(env, want) {
		t.Fatalf("cniEnv() = %#v, want %#v", env, want)
	}
}

func TestProfileHandlersUseConsistentCNIIdentity(t *testing.T) {
	binDir := t.TempDir()
	capturePath := filepath.Join(t.TempDir(), "cni-env")
	t.Setenv("CAPTURE_PATH", capturePath)

	plugin := []byte(`#!/bin/sh
printf '%s|%s|%s|%s\n' "$CNI_COMMAND" "$CNI_CONTAINERID" "$CNI_IFNAME" "$CNI_ARGS" >> "$CAPTURE_PATH"
if [ "$CNI_COMMAND" = "ADD" ]; then
	printf '%s\n' '{"cniVersion":"1.0.0"}'
fi
`)
	if err := os.WriteFile(filepath.Join(binDir, "whereabouts"), plugin, 0o755); err != nil {
		t.Fatalf("write fake whereabouts plugin: %v", err)
	}

	rawConf := []byte(`{"cniVersion":"1.0.0","name":"whereabouts-profile","ipam":{"type":"whereabouts"}}`)
	var conf cniNetConf
	if err := json.Unmarshal(rawConf, &conf); err != nil {
		t.Fatalf("decode test CNI config: %v", err)
	}
	conf.rawBytes = rawConf
	server := &Server{
		binDir: binDir,
		profiles: map[string]cniNetConf{
			"whereabouts-profile": conf,
		},
	}
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", UID: "claim-uid"},
		Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{{
			Resource: "pods",
			Name:     "workload",
			UID:      "pod-uid",
		}}},
	}
	config := &apis.NetworkConfig{Profile: "whereabouts-profile"}

	addRequest := webhook.ProfileRequest{
		Device: cloudprovider.DeviceIdentifiers{Name: "pci-0000-27-00-2"},
		Claim:  claim,
		Config: config,
	}
	callProfileHandler(t, server.GetProfileConfig, addRequest, http.StatusOK)

	releaseRequest := webhook.ProfileReleaseRequest{
		Device:   cloudprovider.DeviceIdentifiers{Name: "pci-0000-27-00-2"},
		ClaimUID: claim.UID,
		Config:   config,
	}
	callProfileHandler(t, server.ReleaseProfileConfig, releaseRequest, http.StatusOK)

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured CNI environment: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(captured)), "\n")
	want := []string{
		"ADD|claim-uid|0000-27-00-2|IgnoreUnknown=1;K8S_POD_NAMESPACE=tenant-a;K8S_POD_NAME=workload;K8S_POD_INFRA_CONTAINER_ID=claim-uid",
		"DEL|claim-uid|0000-27-00-2|IgnoreUnknown=1;K8S_POD_NAMESPACE=default;K8S_POD_NAME=pod-whereabouts;K8S_POD_INFRA_CONTAINER_ID=claim-uid",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("captured CNI environment = %#v, want %#v", got, want)
	}
}

func TestReleaseProfileConfigRequiresClaimUID(t *testing.T) {
	server := &Server{}
	callProfileHandler(t, server.ReleaseProfileConfig, webhook.ProfileReleaseRequest{}, http.StatusBadRequest)
}

func callProfileHandler(t *testing.T, handler http.HandlerFunc, payload any, wantStatus int) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	response := httptest.NewRecorder()
	handler(response, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
	if response.Code != wantStatus {
		t.Fatalf("handler status = %d, want %d; body: %s", response.Code, wantStatus, response.Body.String())
	}
}

func TestCniIfname(t *testing.T) {
	tests := []struct {
		name    string
		devName string
		want    string // exact expected ifname ("" => assert invariants only)
	}{
		{
			name:    "short valid name passes through verbatim",
			devName: "eth0",
			want:    "eth0",
		},
		{
			name:    "15-char name is at the limit and passes through",
			devName: "abcdefghijklmno", // exactly 15
			want:    "abcdefghijklmno",
		},
		{
			name:    "PCI-derived 16-char name keeps the readable BDF (prefix stripped)",
			devName: "pci-0000-27-00-2", // 16 chars, over IFNAMSIZ
			want:    "0000-27-00-2",     // 12 chars, valid, still reads as PCI BDF
		},
		{
			name:    "longest PCI address still fits after stripping",
			devName: "pci-ffff-ff-1f-7",
			want:    "ffff-ff-1f-7",
		},
		{
			name:    "non-PCI over-length name falls back to deterministic hash",
			devName: "net-aaaaaaaaaaaaaaaaaaaa", // base32-style, no "pci-" prefix
			want:    "",                         // opaque hash; checked via invariants
		},
		{
			// Standard Linux PCI addresses never reach this, but a hypothetical
			// over-length PCI name (still >15 after stripping "pci-") must stay
			// correct by degrading to the hash rather than emitting a long name.
			name:    "over-length PCI name still degrades to hash, not an invalid name",
			devName: "pci-00000000-27-00-2", // >15 even after "pci-" is stripped
			want:    "",                     // opaque hash; checked via invariants
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ifname(tt.devName)

			if !isValidCNIIfname(got) {
				t.Fatalf("cniIfname(%q) = %q, which is not a valid CNI ifname", tt.devName, got)
			}
			if tt.want != "" && got != tt.want {
				t.Errorf("cniIfname(%q) = %q, want %q", tt.devName, got, tt.want)
			}
			if tt.want == "" && got == tt.devName {
				t.Errorf("cniIfname(%q) returned the (invalid) name verbatim instead of deriving", tt.devName)
			}
		})
	}
}

// TestCniIfnameDeterministic is the property that actually protects against IP
// leaks: whereabouts matches release on (containerID, ifname), so the same
// device identifier MUST always yield the same CNI_IFNAME (ADD == DEL).
func TestCniIfnameDeterministic(t *testing.T) {
	const dev = "pci-0000-27-00-2"
	first := ifname(dev)
	for i := 0; i < 100; i++ {
		if got := ifname(dev); got != first {
			t.Fatalf("cniIfname(%q) not deterministic: %q != %q", dev, got, first)
		}
	}
}

// TestCniIfnameDistinct ensures different devices in the same claim (same
// containerID, distinct deviceName) do not collide on the derived ifname,
// which would otherwise alias their whereabouts reservations.
func TestCniIfnameDistinct(t *testing.T) {
	names := []string{
		"pci-0000-27-00-2",
		"pci-0000-27-00-3",
		"pci-0000-27-00-4",
		"pci-0000-3b-00-0",
	}
	seen := map[string]string{}
	for _, n := range names {
		got := ifname(n)
		if prev, dup := seen[got]; dup {
			t.Errorf("ifname collision: %q and %q both derive to %q", prev, n, got)
		}
		seen[got] = n
	}
}

func TestIsValidCNIIfname(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"eth0", true},
		{"abcdefghijklmno", true},   // 15 chars
		{"abcdefghijklmnop", false}, // 16 chars
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{"a:b", false},
		{"a b", false},
		{"pci-0000-27-00-2", false}, // 16 chars
		{"dra1a2b3c4d", true},
	}
	for _, tt := range tests {
		if got := isValidCNIIfname(tt.in); got != tt.want {
			t.Errorf("isValidCNIIfname(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
