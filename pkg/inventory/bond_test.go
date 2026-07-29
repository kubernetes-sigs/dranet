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

import "testing"

// TestIsLACPBond_NonBond asserts that a non-existent / non-bond interface
// is reported as not an LACP bond (the sysfs path is absent). The
// positive case (a real 802.3ad bond) is covered by e2e on bond nodes.
func TestIsLACPBond_NonBond(t *testing.T) {
	if IsLACPBond("dranet-definitely-not-a-bond-9999") {
		t.Errorf("expected non-bond interface to report false")
	}
}
