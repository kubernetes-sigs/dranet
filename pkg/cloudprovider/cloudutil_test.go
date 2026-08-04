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

package cloudprovider

import "testing"

func TestIPRangeFromCIDR(t *testing.T) {
	tests := []struct {
		name          string
		cidr          string
		startReserved int
		endReserved   int
		wantStart     string
		wantEnd       string
		wantErr       bool
	}{
		{
			name:      "IPv4 /24 no extra reserve excludes network and broadcast",
			cidr:      "10.0.0.0/24",
			wantStart: "10.0.0.1",
			wantEnd:   "10.0.0.254",
		},
		{
			name:          "IPv4 asymmetric reserve",
			cidr:          "192.168.1.0/28",
			startReserved: 2,
			endReserved:   1,
			wantStart:     "192.168.1.3",
			wantEnd:       "192.168.1.13",
		},
		{
			name:      "input CIDR is not masked",
			cidr:      "10.0.0.7/24",
			wantStart: "10.0.0.1",
			wantEnd:   "10.0.0.254",
		},
		{
			name:      "IPv6 /64 no extra reserve",
			cidr:      "2001:db8::/64",
			wantStart: "2001:db8::1",
			wantEnd:   "2001:db8::ffff:ffff:ffff:fffe",
		},
		{
			name:    "tiny prefix /31 has no allocatable addresses",
			cidr:    "10.0.0.0/31",
			wantErr: true,
		},
		{
			name:          "negative start reserve is rejected",
			cidr:          "10.0.0.0/24",
			startReserved: -1,
			wantErr:       true,
		},
		{
			name:    "invalid CIDR is rejected",
			cidr:    "not-a-cidr",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := IPRangeFromCIDR(tt.cidr, tt.startReserved, tt.endReserved)
			if (err != nil) != tt.wantErr {
				t.Fatalf("IPRangeFromCIDR(%q, %d, %d) error = %v, wantErr %v", tt.cidr, tt.startReserved, tt.endReserved, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("IPRangeFromCIDR(%q, %d, %d) = (%q, %q), want (%q, %q)", tt.cidr, tt.startReserved, tt.endReserved, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}
