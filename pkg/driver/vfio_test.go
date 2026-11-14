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
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/nri/pkg/api"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/features"
	"sigs.k8s.io/dranet/pkg/inventory"
)

const (
	testVFIODriverName = "dra.net"
	testPCI            = "0000:3b:00.2"
	testDevice         = "pci-0000-3b-00-2"
)

type fakePCIDevice struct {
	driver   string
	override string
	native   string
	exists   bool
}

// fakeBinder simulates the kernel side of a rebind: a probe binds the
// override when one is set and the native driver otherwise.
type fakeBinder struct {
	devices           map[string]*fakePCIDevice
	ops               []string
	failOn            map[string]error
	probeBindsNothing bool
	blocker           string
	hostAddresses     []string
	devChars          []LinuxDevice
	// beforeWrite runs before every sysfs write.
	beforeWrite func()
}

func newFakeBinder() *fakeBinder {
	return &fakeBinder{
		devices: map[string]*fakePCIDevice{
			testPCI: {driver: "bnxt_en", native: "bnxt_en", exists: true},
		},
		failOn: map[string]error{},
		devChars: []LinuxDevice{
			{Path: "/dev/vfio/42", Type: "c", Major: 243, Minor: 0},
			{Path: inventory.VFIOContainerDevice, Type: "c", Major: 10, Minor: 196},
		},
	}
}

func (f *fakeBinder) write(op string) error {
	if f.beforeWrite != nil {
		f.beforeWrite()
	}
	f.ops = append(f.ops, op)
	return f.failOn[strings.Fields(op)[0]]
}

func (f *fakeBinder) Exists(pci string) bool     { return f.devices[pci] != nil && f.devices[pci].exists }
func (f *fakeBinder) Driver(pci string) string   { return f.devices[pci].driver }
func (f *fakeBinder) Override(pci string) string { return f.devices[pci].override }
func (f *fakeBinder) Blocker(string) string      { return f.blocker }

func (f *fakeBinder) SetOverride(pci, driver string) error {
	if err := f.write("override " + driver); err != nil {
		return err
	}
	f.devices[pci].override = driver
	return nil
}

func (f *fakeBinder) Unbind(pci string) error {
	if err := f.write("unbind"); err != nil {
		return err
	}
	f.devices[pci].driver = ""
	return nil
}

func (f *fakeBinder) Probe(pci string) error {
	if err := f.write("probe"); err != nil {
		return err
	}
	d := f.devices[pci]
	switch {
	case d.driver != "":
	case f.probeBindsNothing && d.override == inventory.VFIODriver:
	case d.override != "":
		d.driver = d.override
	default:
		d.driver = d.native
	}
	return nil
}

func (f *fakeBinder) HostAddresses(string) ([]string, error) { return f.hostAddresses, nil }
func (f *fakeBinder) DevChars(string) ([]LinuxDevice, error) { return f.devChars, nil }

func vfioClaim(podUID types.UID, device string) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: "claim-uid", Namespace: "default", Name: "vm-nic"},
		Status: resourcev1.ResourceClaimStatus{
			ReservedFor: []resourcev1.ResourceClaimConsumerReference{{Resource: "pods", Name: "virt-launcher", UID: podUID}},
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{
						{Driver: testVFIODriverName, Pool: "node-a", Device: device, Request: "nic"},
					},
					Config: []resourcev1.DeviceAllocationConfiguration{{
						Source: resourcev1.AllocationConfigSourceClass,
						DeviceConfiguration: resourcev1.DeviceConfiguration{
							Opaque: &resourcev1.OpaqueDeviceConfiguration{
								Driver:     testVFIODriverName,
								Parameters: runtime.RawExtension{Raw: []byte(`{"vfio":{}}`)},
							},
						},
					}},
				},
			},
		},
	}
}

func newVFIOTestDriver(t *testing.T, b *fakeBinder) (*NetworkDriver, *fakeInventoryDB) {
	t.Helper()
	store, err := NewPodConfigStore("")
	if err != nil {
		t.Fatal(err)
	}
	db := newFakeInventoryDB()
	db.GetDeviceFunc = func(name string) (resourcev1.Device, bool) {
		return resourcev1.Device{
			Name:       name,
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{apis.AttrPCIAddress: {StringValue: ptr.To(testPCI)}},
		}, true
	}
	return &NetworkDriver{
		driverName:     testVFIODriverName,
		nodeName:       "node-a",
		netdb:          db,
		podConfigStore: store,
		eventRecorder:  record.NewFakeRecorder(10),
		vfioBinder:     b,
	}, db
}

func prepareVFIO(np *NetworkDriver, claim *resourcev1.ResourceClaim) error {
	return np.prepareDevice(context.Background(), nlwrap.Handle{}, claim, claim.Status.ReservedFor[0].UID,
		claim.Status.Allocation.Devices.Results[0], nil)
}

func enableVFIO(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, features.DefaultMutableFeatureGate, features.VFIODriverRebind, true)
}

func TestVFIOBindOrder(t *testing.T) {
	b := newFakeBinder()
	if err := vfioBind(b, testPCI); err != nil {
		t.Fatal(err)
	}
	want := []string{"override vfio-pci", "unbind", "probe"}
	if !reflect.DeepEqual(b.ops, want) {
		t.Errorf("ops = %v, want %v", b.ops, want)
	}
	if d := b.devices[testPCI]; d.driver != inventory.VFIODriver || d.override != inventory.VFIODriver {
		t.Errorf("device = %+v, want bound to vfio-pci with the override kept", d)
	}
}

func TestVFIOBindRollbackClearsOverride(t *testing.T) {
	tests := []struct {
		name  string
		setup func(b *fakeBinder)
	}{
		{name: "probe binds nothing", setup: func(b *fakeBinder) { b.probeBindsNothing = true }},
		{name: "unbind fails", setup: func(b *fakeBinder) { b.failOn["unbind"] = errors.New("busy") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newFakeBinder()
			tt.setup(b)
			if err := vfioBind(b, testPCI); err == nil {
				t.Fatal("vfioBind() succeeded, want an error")
			}
			d := b.devices[testPCI]
			if d.override != "" {
				t.Errorf("override = %q after a failed bind, want it cleared", d.override)
			}
			if d.driver != "bnxt_en" {
				t.Errorf("driver = %q after a failed bind, want the original bnxt_en", d.driver)
			}
		})
	}
}

func TestVFIORestore(t *testing.T) {
	tests := []struct {
		name       string
		device     *fakePCIDevice
		failProbe  bool
		wantErr    bool
		wantDriver string
		wantOps    []string
	}{
		{
			name:       "bound to vfio-pci",
			device:     &fakePCIDevice{driver: "vfio-pci", override: "vfio-pci", native: "bnxt_en", exists: true},
			wantDriver: "bnxt_en",
			wantOps:    []string{"override ", "unbind", "probe"},
		},
		{
			name:       "driverless with a stale override",
			device:     &fakePCIDevice{override: "vfio-pci", native: "bnxt_en", exists: true},
			wantDriver: "bnxt_en",
			wantOps:    []string{"override ", "probe"},
		},
		{
			name:       "already on the original driver",
			device:     &fakePCIDevice{driver: "bnxt_en", native: "bnxt_en", exists: true},
			wantDriver: "bnxt_en",
		},
		{
			name:   "device gone after a PF driver reload",
			device: &fakePCIDevice{exists: false},
		},
		{
			name:      "probe fails",
			device:    &fakePCIDevice{driver: "vfio-pci", override: "vfio-pci", native: "bnxt_en", exists: true},
			failProbe: true,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newFakeBinder()
			b.devices[testPCI] = tt.device
			if tt.failProbe {
				b.failOn["probe"] = errors.New("no driver")
			}
			err := vfioRestore(b, testPCI, "bnxt_en")
			if (err != nil) != tt.wantErr {
				t.Fatalf("vfioRestore() = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(b.ops, tt.wantOps) {
				t.Errorf("ops = %v, want %v", b.ops, tt.wantOps)
			}
			if tt.device.exists && b.devices[testPCI].driver != tt.wantDriver {
				t.Errorf("driver = %q, want %q", b.devices[testPCI].driver, tt.wantDriver)
			}
		})
	}
}

func TestPrepareVFIORequiresFeatureGate(t *testing.T) {
	b := newFakeBinder()
	np, _ := newVFIOTestDriver(t, b)
	err := prepareVFIO(np, vfioClaim("pod-1", testDevice))
	if err == nil || !strings.Contains(err.Error(), "VFIODriverRebind") {
		t.Fatalf("prepare = %v, want the feature gate error", err)
	}
	if len(b.ops) != 0 {
		t.Errorf("sysfs written with the gate disabled: %v", b.ops)
	}
}

func TestPrepareVFIORecordsOriginalDriverBeforeFirstWrite(t *testing.T) {
	enableVFIO(t)
	b := newFakeBinder()
	np, _ := newVFIOTestDriver(t, b)
	checked := false
	b.beforeWrite = func() {
		if checked {
			return
		}
		checked = true
		cfg, ok := np.podConfigStore.GetDeviceConfig("pod-1", testDevice)
		if !ok || cfg.VFIO == nil || cfg.VFIO.OriginalDriver != "bnxt_en" || cfg.VFIO.Bound {
			t.Errorf("first sysfs write happened before the original driver was checkpointed: %+v", cfg.VFIO)
		}
	}
	if err := prepareVFIO(np, vfioClaim("pod-1", testDevice)); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("prepare did not write sysfs")
	}
}

func TestPrepareVFIO(t *testing.T) {
	enableVFIO(t)
	b := newFakeBinder()
	np, _ := newVFIOTestDriver(t, b)
	claim := vfioClaim("pod-1", testDevice)
	if err := prepareVFIO(np, claim); err != nil {
		t.Fatal(err)
	}
	cfg, ok := np.podConfigStore.GetDeviceConfig("pod-1", testDevice)
	if !ok || cfg.VFIO == nil {
		t.Fatalf("no VFIO record after prepare: %+v", cfg)
	}
	want := VFIOState{PCIAddress: testPCI, OriginalDriver: "bnxt_en", Bound: true, DevChars: b.devChars}
	if !reflect.DeepEqual(*cfg.VFIO, want) {
		t.Errorf("VFIO record = %+v, want %+v", *cfg.VFIO, want)
	}
	if cfg.NetworkInterfaceConfigInHost.Interface.Name != "" || cfg.NetworkInterfaceConfigInPod.Interface.Name != "" {
		t.Errorf("VFIO device has an interface name, the NRI hooks would treat it as a netdev: %+v", cfg)
	}
	if b.devices[testPCI].driver != inventory.VFIODriver {
		t.Errorf("driver = %q, want vfio-pci", b.devices[testPCI].driver)
	}

	devices := np.vfioPrepareResult(claim, "pod-1")
	if len(devices) != 1 || devices[0].Metadata == nil {
		t.Fatalf("PrepareResult devices = %+v, want one device with metadata", devices)
	}
	got := devices[0].Metadata.Attributes["resource.kubernetes.io/pciBusID"]
	if got.StringValue == nil || *got.StringValue != testPCI {
		t.Errorf("pciBusID metadata = %v, want %s", got.StringValue, testPCI)
	}
	if devices[0].PoolName != "node-a" || devices[0].DeviceName != testDevice || !reflect.DeepEqual(devices[0].Requests, []string{"nic"}) {
		t.Errorf("PrepareResult device = %+v", devices[0])
	}

	opsAfterPrepare := len(b.ops)
	if err := prepareVFIO(np, claim); err != nil {
		t.Fatalf("repeated prepare = %v", err)
	}
	if len(b.ops) != opsAfterPrepare {
		t.Errorf("repeated prepare wrote sysfs again: %v", b.ops[opsAfterPrepare:])
	}
}

func TestPrepareVFIORefusesUnsafeDevices(t *testing.T) {
	enableVFIO(t)
	tests := []struct {
		name    string
		setup   func(b *fakeBinder)
		wantErr string
	}{
		{
			name:    "shared IOMMU group",
			setup:   func(b *fakeBinder) { b.blocker = "its IOMMU group 42 is shared with 0000:3b:00.3 (bnxt_en)" },
			wantErr: "IOMMU group 42 is shared",
		},
		{
			name:    "host addresses",
			setup:   func(b *fakeBinder) { b.hostAddresses = []string{"10.0.0.5/24"} },
			wantErr: "host addresses",
		},
		{
			name:    "already on vfio-pci without a record",
			setup:   func(b *fakeBinder) { b.devices[testPCI].driver = inventory.VFIODriver },
			wantErr: "not to a kernel network driver",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newFakeBinder()
			tt.setup(b)
			np, _ := newVFIOTestDriver(t, b)
			err := prepareVFIO(np, vfioClaim("pod-1", testDevice))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("prepare = %v, want an error containing %q", err, tt.wantErr)
			}
			if len(b.ops) != 0 {
				t.Errorf("sysfs written for a refused device: %v", b.ops)
			}
			if _, ok := np.podConfigStore.GetDeviceConfig("pod-1", testDevice); ok {
				t.Errorf("record stored for a refused device")
			}
		})
	}
}

func TestPrepareVFIOResumesInterruptedBind(t *testing.T) {
	enableVFIO(t)
	b := newFakeBinder()
	b.devices[testPCI] = &fakePCIDevice{override: inventory.VFIODriver, native: "bnxt_en", exists: true}
	np, _ := newVFIOTestDriver(t, b)
	claim := vfioClaim("pod-1", testDevice)
	if err := np.podConfigStore.SetDeviceConfig("pod-1", testDevice, DeviceConfig{
		Claim:                       types.NamespacedName{Namespace: "default", Name: "vm-nic"},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{VFIO: &apis.VFIOConfig{}},
		VFIO:                        &VFIOState{PCIAddress: testPCI, OriginalDriver: "bnxt_en"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := prepareVFIO(np, claim); err != nil {
		t.Fatal(err)
	}
	cfg, _ := np.podConfigStore.GetDeviceConfig("pod-1", testDevice)
	if !cfg.VFIO.Bound || cfg.VFIO.OriginalDriver != "bnxt_en" || b.devices[testPCI].driver != inventory.VFIODriver {
		t.Errorf("resumed prepare: record %+v, driver %q", cfg.VFIO, b.devices[testPCI].driver)
	}
}

func TestPrepareVFIOFailedBindKeepsRecordForRelease(t *testing.T) {
	enableVFIO(t)
	b := newFakeBinder()
	b.probeBindsNothing = true
	np, _ := newVFIOTestDriver(t, b)
	claim := vfioClaim("pod-1", testDevice)
	if err := prepareVFIO(np, claim); err == nil {
		t.Fatal("prepare succeeded, want an error")
	}
	cfg, ok := np.podConfigStore.GetDeviceConfig("pod-1", testDevice)
	if !ok || cfg.VFIO == nil || cfg.VFIO.Bound || cfg.VFIO.OriginalDriver != "bnxt_en" {
		t.Fatalf("record after a failed bind = %+v, want an unconfirmed record with the original driver", cfg.VFIO)
	}
	b.probeBindsNothing = false
	if err := np.unprepareResourceClaim(context.Background(), kubeletplugin.NamespacedObject{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "vm-nic"}, UID: "claim-uid",
	}); err != nil {
		t.Fatal(err)
	}
	if b.devices[testPCI].driver != "bnxt_en" || b.devices[testPCI].override != "" {
		t.Errorf("device after release = %+v, want bnxt_en without override", b.devices[testPCI])
	}
}

func TestUnprepareVFIO(t *testing.T) {
	enableVFIO(t)
	claimRef := kubeletplugin.NamespacedObject{NamespacedName: types.NamespacedName{Namespace: "default", Name: "vm-nic"}, UID: "claim-uid"}

	t.Run("restores the original driver", func(t *testing.T) {
		b := newFakeBinder()
		np, db := newVFIOTestDriver(t, b)
		if err := prepareVFIO(np, vfioClaim("pod-1", testDevice)); err != nil {
			t.Fatal(err)
		}
		if err := np.unprepareResourceClaim(context.Background(), claimRef); err != nil {
			t.Fatal(err)
		}
		if d := b.devices[testPCI]; d.driver != "bnxt_en" || d.override != "" {
			t.Errorf("device after unprepare = %+v, want bnxt_en without override", d)
		}
		if _, ok := np.podConfigStore.GetDeviceConfig("pod-1", testDevice); ok {
			t.Errorf("record kept after a successful restore")
		}
		if db.rescanCalls.Load() == 0 {
			t.Errorf("no rescan requested after the function returned to its kernel driver")
		}
	})

	t.Run("a failed restore keeps the record and is retried", func(t *testing.T) {
		b := newFakeBinder()
		np, _ := newVFIOTestDriver(t, b)
		if err := prepareVFIO(np, vfioClaim("pod-1", testDevice)); err != nil {
			t.Fatal(err)
		}
		b.failOn["probe"] = errors.New("bnxt_en probe failed")
		if err := np.unprepareResourceClaim(context.Background(), claimRef); err == nil {
			t.Fatal("unprepare succeeded, want the restore error so kubelet retries")
		}
		cfg, ok := np.podConfigStore.GetDeviceConfig("pod-1", testDevice)
		if !ok || cfg.VFIO == nil || cfg.VFIO.OriginalDriver != "bnxt_en" {
			t.Fatalf("record after a failed restore = %+v, want it kept", cfg.VFIO)
		}
		delete(b.failOn, "probe")
		if err := np.unprepareResourceClaim(context.Background(), claimRef); err != nil {
			t.Fatalf("retried unprepare = %v", err)
		}
		if b.devices[testPCI].driver != "bnxt_en" {
			t.Errorf("driver after the retry = %q, want bnxt_en", b.devices[testPCI].driver)
		}
	})

	t.Run("a function removed by a PF driver reload", func(t *testing.T) {
		b := newFakeBinder()
		np, _ := newVFIOTestDriver(t, b)
		if err := prepareVFIO(np, vfioClaim("pod-1", testDevice)); err != nil {
			t.Fatal(err)
		}
		b.devices[testPCI].exists = false
		opsBefore := len(b.ops)
		if err := np.unprepareResourceClaim(context.Background(), claimRef); err != nil {
			t.Fatal(err)
		}
		if len(b.ops) != opsBefore {
			t.Errorf("sysfs written for a function that no longer exists: %v", b.ops[opsBefore:])
		}
		if _, ok := np.podConfigStore.GetDeviceConfig("pod-1", testDevice); ok {
			t.Errorf("record kept for a function that no longer exists")
		}
	})

	t.Run("a pod with two vfio claims gets both functions back", func(t *testing.T) {
		const otherPCI, otherDevice = "0000:3b:00.3", "pci-0000-3b-00-3"
		b := newFakeBinder()
		b.devices[otherPCI] = &fakePCIDevice{driver: "bnxt_en", native: "bnxt_en", exists: true}
		np, db := newVFIOTestDriver(t, b)
		db.GetDeviceFunc = func(name string) (resourcev1.Device, bool) {
			pci := testPCI
			if name == otherDevice {
				pci = otherPCI
			}
			return resourcev1.Device{Name: name, Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				apis.AttrPCIAddress: {StringValue: ptr.To(pci)},
			}}, true
		}
		first := vfioClaim("pod-1", testDevice)
		second := vfioClaim("pod-1", otherDevice)
		second.Name, second.UID = "vm-nic-2", "claim-uid-2"
		for _, c := range []*resourcev1.ResourceClaim{first, second} {
			if err := prepareVFIO(np, c); err != nil {
				t.Fatal(err)
			}
		}
		if err := np.unprepareResourceClaim(context.Background(), claimRef); err != nil {
			t.Fatal(err)
		}
		if err := np.unprepareResourceClaim(context.Background(), kubeletplugin.NamespacedObject{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: "vm-nic-2"}, UID: "claim-uid-2",
		}); err != nil {
			t.Fatal(err)
		}
		for _, pci := range []string{testPCI, otherPCI} {
			if d := b.devices[pci]; d.driver != "bnxt_en" || d.override != "" {
				t.Errorf("%s after unprepare = %+v, want bnxt_en without override", pci, d)
			}
		}
	})

	t.Run("a failed restore releases nothing else", func(t *testing.T) {
		b := newFakeBinder()
		np, db := newVFIOTestDriver(t, b)
		if err := prepareVFIO(np, vfioClaim("pod-1", testDevice)); err != nil {
			t.Fatal(err)
		}
		if err := np.podConfigStore.SetDeviceConfig("pod-1", "eth-dev", DeviceConfig{
			Claim:                       types.NamespacedName{Namespace: "default", Name: "vm-nic"},
			NetworkInterfaceConfigInPod: apis.NetworkConfig{Profile: "default"},
		}); err != nil {
			t.Fatal(err)
		}
		b.failOn["probe"] = errors.New("bnxt_en probe failed")
		if err := np.unprepareResourceClaim(context.Background(), claimRef); err == nil {
			t.Fatal("unprepare succeeded, want the restore error")
		}
		if n := db.releaseProfileCalls.Load(); n != 0 {
			t.Errorf("profile released %d times before the restore succeeded, want 0 so a retry does not release it twice", n)
		}
	})

	t.Run("works with the gate disabled after a downgrade", func(t *testing.T) {
		b := newFakeBinder()
		np, _ := newVFIOTestDriver(t, b)
		if err := prepareVFIO(np, vfioClaim("pod-1", testDevice)); err != nil {
			t.Fatal(err)
		}
		featuregatetesting.SetFeatureGateDuringTest(t, features.DefaultMutableFeatureGate, features.VFIODriverRebind, false)
		if err := np.unprepareResourceClaim(context.Background(), claimRef); err != nil {
			t.Fatal(err)
		}
		if b.devices[testPCI].driver != "bnxt_en" {
			t.Errorf("driver = %q, want bnxt_en", b.devices[testPCI].driver)
		}
	})
}

func TestVFIOPrepareResultOnlyForVFIOClaims(t *testing.T) {
	b := newFakeBinder()
	np, _ := newVFIOTestDriver(t, b)
	claim := vfioClaim("pod-1", "eth-dev")
	if err := np.podConfigStore.SetDeviceConfig("pod-1", "eth-dev", DeviceConfig{
		NetworkInterfaceConfigInHost: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "eth1"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := np.vfioPrepareResult(claim, "pod-1"); got != nil {
		t.Errorf("vfioPrepareResult() for a netdev claim = %+v, want nil", got)
	}
}

func TestCreateContainerInjectsVFIODevices(t *testing.T) {
	np := &NetworkDriver{}
	group := func(n int64) LinuxDevice {
		return LinuxDevice{Path: fmt.Sprintf("/dev/vfio/%d", n), Type: "c", Major: 243, Minor: n}
	}
	container := LinuxDevice{Path: inventory.VFIOContainerDevice, Type: "c", Major: 10, Minor: 196}
	podConfig := PodConfig{DeviceConfigs: map[string]DeviceConfig{
		"dev-a": {VFIO: &VFIOState{Bound: true, DevChars: []LinuxDevice{group(42), container}}},
		"dev-b": {VFIO: &VFIOState{Bound: true, DevChars: []LinuxDevice{group(43), container}}},
	}}
	adjust, _, err := np.createContainer(context.Background(), &api.PodSandbox{}, &api.Container{}, podConfig)
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, d := range adjust.Linux.Devices {
		count[d.Path]++
	}
	want := map[string]int{"/dev/vfio/42": 1, "/dev/vfio/43": 1, inventory.VFIOContainerDevice: 1}
	if !reflect.DeepEqual(count, want) {
		t.Errorf("injected devices = %v, want %v", count, want)
	}
}
