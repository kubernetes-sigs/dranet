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
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/pkg/apis"
)

type fakePCI struct {
	t    *testing.T
	root string
}

func newFakePCI(t *testing.T) *fakePCI {
	t.Helper()
	f := &fakePCI{t: t, root: t.TempDir()}
	f.driver("bnxt_en")
	f.driver(VFIODriver)
	return f
}

func (f *fakePCI) write(rel, content string) {
	f.t.Helper()
	path := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakePCI) symlink(target, rel string) {
	f.t.Helper()
	path := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakePCI) driver(name string) {
	f.write(filepath.Join(sysBusPCIDrivers, name, "unbind"), "")
}

// device adds a PCI function in the given IOMMU group, bound to driver
// ("" for unbound).
func (f *fakePCI) device(bdf, driver, group string) {
	dev := filepath.Join(sysBusPCIDevices, bdf)
	f.write(filepath.Join(dev, "driver_override"), "(null)\n")
	if driver != "" {
		f.symlink("../../../bus/pci/drivers/"+driver, filepath.Join(dev, "driver"))
	}
	if group != "" {
		f.symlink("../../../kernel/iommu_groups/"+group, filepath.Join(dev, "iommu_group"))
		f.write(filepath.Join(sysKernelIOMMUGroups, group, "devices", bdf), "")
	}
}

func TestVFIOBlocker(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(f *fakePCI)
		blocked string
	}{
		{
			name: "alone in its IOMMU group",
			setup: func(f *fakePCI) {
				f.device("0000:3b:00.2", "bnxt_en", "42")
			},
		},
		{
			name: "group shared with a PCI bridge and an unbound function",
			setup: func(f *fakePCI) {
				f.device("0000:3b:00.2", "bnxt_en", "42")
				f.device("0000:3a:00.0", "pcieport", "42")
				f.device("0000:3b:00.3", "", "42")
			},
		},
		{
			name: "group shared with a function parked on pci-stub",
			setup: func(f *fakePCI) {
				f.device("0000:3b:00.2", "bnxt_en", "42")
				f.device("0000:3b:00.3", "pci-stub", "42")
			},
		},
		{
			name: "group shared with a bridge on a driver that does DMA",
			setup: func(f *fakePCI) {
				f.device("0000:3b:00.2", "bnxt_en", "42")
				f.device("0000:00:1f.0", "lpc_ich", "42")
			},
			blocked: "shared with 0000:00:1f.0 (lpc_ich)",
		},
		{
			name: "vfio-pci not loaded",
			setup: func(f *fakePCI) {
				f.device("0000:3b:00.2", "bnxt_en", "42")
				if err := os.RemoveAll(filepath.Join(f.root, sysBusPCIDrivers, VFIODriver)); err != nil {
					t.Fatal(err)
				}
			},
			blocked: "vfio-pci driver is not loaded",
		},
		{
			name: "PF with virtual functions",
			setup: func(f *fakePCI) {
				f.device("0000:3b:00.0", "bnxt_en", "40")
				f.symlink("../0000:3b:00.2", filepath.Join(sysBusPCIDevices, "0000:3b:00.0", "virtfn0"))
			},
			blocked: "virtual functions",
		},
		{
			name: "network interface enslaved to a bond",
			setup: func(f *fakePCI) {
				f.device("0000:3b:00.2", "bnxt_en", "42")
				f.symlink("../../../../virtual/net/bond0", filepath.Join(sysBusPCIDevices, "0000:3b:00.2", "net", "ens1f0", "upper_bond0"))
			},
			blocked: "part of bond0",
		},
		{
			name: "network interface carrying a VLAN",
			setup: func(f *fakePCI) {
				f.device("0000:3b:00.2", "bnxt_en", "42")
				f.symlink("../../../../virtual/net/ens1f0.100", filepath.Join(sysBusPCIDevices, "0000:3b:00.2", "net", "ens1f0", "upper_ens1f0.100"))
			},
			blocked: "part of ens1f0.100",
		},
		{
			name: "no IOMMU group",
			setup: func(f *fakePCI) {
				f.device("0000:3b:00.2", "bnxt_en", "")
			},
			blocked: "no IOMMU group",
		},
		{
			name: "group shared with a function on another driver",
			setup: func(f *fakePCI) {
				f.device("0000:3b:00.2", "bnxt_en", "42")
				f.device("0000:3b:00.3", "bnxt_en", "42")
			},
			blocked: "shared with 0000:3b:00.3 (bnxt_en)",
		},
		{
			name: "group shared with a function already on vfio-pci",
			setup: func(f *fakePCI) {
				f.device("0000:3b:00.2", "bnxt_en", "42")
				f.device("0000:3b:00.3", VFIODriver, "42")
			},
			blocked: "shared with 0000:3b:00.3 (vfio-pci)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakePCI(t)
			tt.setup(f)
			bdf := "0000:3b:00.2"
			if strings.Contains(tt.name, "PF") {
				bdf = "0000:3b:00.0"
			}
			got := VFIOBlocker(f.root, bdf)
			if tt.blocked == "" && got != "" {
				t.Errorf("VFIOBlocker() = %q, want eligible", got)
			}
			if tt.blocked != "" && !strings.Contains(got, tt.blocked) {
				t.Errorf("VFIOBlocker() = %q, want it to contain %q", got, tt.blocked)
			}
		})
	}
}

func TestAddPCIAttributesVFIO(t *testing.T) {
	newDevice := func(extra map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) []hostDevice {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			apis.AttrPCIAddress: {StringValue: ptr.To("0000:3b:00.2")},
		}
		for k, v := range extra {
			attrs[k] = v
		}
		return []hostDevice{{Device: resourceapi.Device{Name: "pci-0000-3b-00-2", Attributes: attrs}}}
	}

	t.Run("gate disabled publishes neither attribute", func(t *testing.T) {
		f := newFakePCI(t)
		f.device("0000:3b:00.2", "bnxt_en", "42")
		db := New()
		devices := db.addPCIAttributes(newDevice(nil), nil)
		if _, ok := devices[0].Attributes[apis.AttrVFIO]; ok {
			t.Errorf("dra.net/vfio published with the gate disabled")
		}
		if _, ok := devices[0].Attributes["resource.kubernetes.io/pciBusID"]; ok {
			t.Errorf("pciBusID published with the gate disabled")
		}
	})

	t.Run("macvlan on an eligible function publishes neither attribute", func(t *testing.T) {
		f := newFakePCI(t)
		f.device("0000:3b:00.2", "bnxt_en", "42")
		db := New(WithVFIOAttributes(f.root))
		devices := db.addPCIAttributes([]hostDevice{{
			Device:           resourceapi.Device{Name: "macvlan0", Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}},
			parentPCIAddress: "0000:3b:00.2",
		}}, nil)
		if _, ok := devices[0].Attributes[apis.AttrVFIO]; ok {
			t.Errorf("dra.net/vfio published for a macvlan, a vfio claim on it would rebind its parent")
		}
		if _, ok := devices[0].Attributes["resource.kubernetes.io/pciBusID"]; ok {
			t.Errorf("pciBusID published for a macvlan")
		}
	})

	tests := []struct {
		name  string
		extra map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
		group string
		want  bool
	}{
		{name: "eligible function", group: "42", want: true},
		{name: "function without IOMMU group", group: "", want: false},
		{
			name:  "function with a host IPv4 address",
			group: "42",
			extra: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{apis.AttrIPv4: {StringValue: ptr.To("10.0.0.5")}},
			want:  false,
		},
		{
			name:  "function with a host IPv6 address",
			group: "42",
			extra: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{apis.AttrIPv6: {StringValue: ptr.To("2001:db8::5")}},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakePCI(t)
			f.device("0000:3b:00.2", "bnxt_en", tt.group)
			db := New(WithVFIOAttributes(f.root))
			devices := db.addPCIAttributes(newDevice(tt.extra), nil)
			got := devices[0].Attributes[apis.AttrVFIO]
			if got.BoolValue == nil || *got.BoolValue != tt.want {
				t.Errorf("dra.net/vfio = %v, want %v", got.BoolValue, tt.want)
			}
			bus := devices[0].Attributes["resource.kubernetes.io/pciBusID"]
			if bus.StringValue == nil || *bus.StringValue != "0000:3b:00.2" {
				t.Errorf("pciBusID = %v, want 0000:3b:00.2", bus.StringValue)
			}
		})
	}
}
