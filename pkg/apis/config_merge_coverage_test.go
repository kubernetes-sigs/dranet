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

package apis

import (
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/utils/ptr"
)

// These tests walk the NetworkConfig type with reflection, so a new field is
// covered without a field list. Every leaf gets a value that names its own path,
// so a merge that reads the wrong field shows up as a difference.

// leaf is one settable value: a scalar, a pointer to a scalar, a slice, a map,
// or a pointer to an empty struct (presence-only marker).
type leaf struct {
	path string
	// get returns the leaf inside root and allocates struct pointers on the way.
	get func(root reflect.Value) reflect.Value
}

func collectLeaves(t reflect.Type, path string, get func(reflect.Value) reflect.Value) []leaf {
	switch t.Kind() {
	case reflect.Struct:
		var out []leaf
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			childPath := field.Name
			if path != "" {
				childPath = path + "." + field.Name
			}
			out = append(out, collectLeaves(field.Type, childPath, func(root reflect.Value) reflect.Value {
				return get(root).Field(i)
			})...)
		}
		return out
	case reflect.Pointer:
		if t.Elem().Kind() != reflect.Struct || t.Elem().Size() == 0 {
			return []leaf{{path: path, get: get}}
		}
		return collectLeaves(t.Elem(), path, func(root reflect.Value) reflect.Value {
			v := get(root)
			if v.IsNil() {
				v.Set(reflect.New(t.Elem()))
			}
			return v.Elem()
		})
	case reflect.String, reflect.Bool, reflect.Slice, reflect.Map,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return []leaf{{path: path, get: get}}
	default:
		panic("collectLeaves: unsupported kind " + t.Kind().String() + " at " + path)
	}
}

// structPtrNode identifies every struct-pointer field at any depth in a struct
// tree, including empty structs (presence-only markers) and non-empty nested
// blocks that can arrive as empty non-nil objects (e.g., vrf: {} or ipvlan: {}).
type structPtrNode struct {
	path string
	get  func(root reflect.Value) reflect.Value
}

func collectStructPointers(t reflect.Type, path string, get func(reflect.Value) reflect.Value) []structPtrNode {
	switch t.Kind() {
	case reflect.Struct:
		var out []structPtrNode
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			childPath := field.Name
			if path != "" {
				childPath = path + "." + field.Name
			}
			out = append(out, collectStructPointers(field.Type, childPath, func(root reflect.Value) reflect.Value {
				return get(root).Field(i)
			})...)
		}
		return out
	case reflect.Pointer:
		if t.Elem().Kind() != reflect.Struct {
			return nil
		}
		out := []structPtrNode{{path: path, get: get}}
		out = append(out, collectStructPointers(t.Elem(), path, func(root reflect.Value) reflect.Value {
			v := get(root)
			if v.IsNil() {
				v.Set(reflect.New(t.Elem()))
			}
			return v.Elem()
		})...)
		return out
	default:
		return nil
	}
}

var (
	networkConfigLeaves     = collectLeaves(reflect.TypeOf(NetworkConfig{}), "", func(root reflect.Value) reflect.Value { return root })
	networkConfigStructPtrs = collectStructPointers(reflect.TypeOf(NetworkConfig{}), "", func(root reflect.Value) reflect.Value { return root })
)

// setValue writes a value into v. Strings carry marker and path, numbers get
// distinct sequential values from nextN, bools are true, and slices get spare
// capacity (cap > len) so appends without cloning mutate inputs. With zero set,
// pointers are allocated but every pointed-to value is zero (slices and maps
// remain nil).
func setValue(v reflect.Value, path, marker string, nextN *int64, zero bool) {
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		setValue(v.Elem(), path, marker, nextN, zero)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			setValue(v.Field(i), path+"."+field.Name, marker, nextN, zero)
		}
	case reflect.String:
		if !zero {
			v.SetString(marker + ":" + path)
		}
	case reflect.Bool:
		v.SetBool(!zero)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if zero {
			v.SetInt(0)
			return
		}
		*nextN++
		if *nextN > math.MaxInt8 {
			panic(fmt.Sprintf("setValue: numeric value %d at %s does not fit in an int8", *nextN, path))
		}
		v.SetInt(*nextN)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if zero {
			v.SetUint(0)
			return
		}
		*nextN++
		if *nextN > math.MaxInt8 {
			panic(fmt.Sprintf("setValue: numeric value %d at %s does not fit in an int8", *nextN, path))
		}
		v.SetUint(uint64(*nextN))
	case reflect.Slice:
		if zero {
			return
		}
		elem := reflect.New(v.Type().Elem()).Elem()
		setValue(elem, path, marker, nextN, false)
		s := reflect.MakeSlice(v.Type(), 0, 4)
		v.Set(reflect.Append(s, elem))
	case reflect.Map:
		if zero {
			return
		}
		key := reflect.New(v.Type().Key()).Elem()
		setValue(key, path+"[key]", marker, nextN, false)
		val := reflect.New(v.Type().Elem()).Elem()
		setValue(val, path+"[value]", marker, nextN, false)
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(key, val)
		v.Set(m)
	default:
		panic("setValue: unsupported kind " + v.Kind().String() + " at " + path)
	}
}

// fullConfig sets every leaf. Every numeric field receives a distinct number
// starting after offset (fitting within int8/uint8), so two configs with
// well-separated offsets share no number and sibling struct fields differ.
func fullConfig(marker string, offset int64) *NetworkConfig {
	config := &NetworkConfig{}
	root := reflect.ValueOf(config).Elem()
	nextN := offset
	for _, l := range networkConfigLeaves {
		setValue(l.get(root), l.path, marker, &nextN, false)
	}
	return config
}

// zeroConfig allocates every pointer but leaves every value zero.
func zeroConfig() *NetworkConfig {
	config := &NetworkConfig{}
	root := reflect.ValueOf(config).Elem()
	var n int64
	for _, l := range networkConfigLeaves {
		setValue(l.get(root), l.path, "", &n, true)
	}
	return config
}

// singleLeafConfig sets one leaf only.
func singleLeafConfig(l leaf, n int64) *NetworkConfig {
	config := &NetworkConfig{}
	nextN := n - 1
	setValue(l.get(reflect.ValueOf(config).Elem()), l.path, "x", &nextN, false)
	return config
}

// emptyStructPtrConfig sets one struct-pointer node to a non-nil empty struct.
func emptyStructPtrConfig(node structPtrNode) *NetworkConfig {
	config := &NetworkConfig{}
	field := node.get(reflect.ValueOf(config).Elem())
	field.Set(reflect.New(field.Type().Elem()))
	return config
}

// A value set on one side only must survive the merge unchanged, for every
// field, and the merge must not change its input.
func TestMergeKeepsEveryFieldFromEitherSide(t *testing.T) {
	want := fullConfig("x", 0)
	for _, tc := range []struct {
		name  string
		merge func(input *NetworkConfig) *NetworkConfig
	}{
		{"user only", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(in, &NetworkConfig{}) }},
		{"cloud only", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(&NetworkConfig{}, in) }},
		{"user only, nil cloud", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(in, nil) }},
		{"cloud only, nil user", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(nil, in) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := fullConfig("x", 0)
			if diff := cmp.Diff(want, tc.merge(input)); diff != "" {
				t.Errorf("merge lost or changed a field (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(want, input); diff != "" {
				t.Errorf("merge changed its input (-want +got):\n%s", diff)
			}
		})
	}
}

// One leaf at a time: the result must hold that leaf and nothing else, and must
// not share memory with the input. This catches a merge that reads a neighbor
// field, which whole-config fixtures cannot see when both fields hold the same
// value.
func TestMergeKeepsEachFieldAlone(t *testing.T) {
	for i, l := range networkConfigLeaves {
		want := singleLeafConfig(l, int64(i)+1)
		for _, tc := range []struct {
			name  string
			merge func(input *NetworkConfig) *NetworkConfig
		}{
			{"user only", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(in, &NetworkConfig{}) }},
			{"user only, nil cloud", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(in, nil) }},
			{"cloud only", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(&NetworkConfig{}, in) }},
			{"cloud only, nil user", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(nil, in) }},
			{"clone", func(in *NetworkConfig) *NetworkConfig { return in.Clone() }},
		} {
			t.Run(l.path+"/"+tc.name, func(t *testing.T) {
				input := singleLeafConfig(l, int64(i)+1)
				got := tc.merge(input)
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("(-want +got):\n%s", diff)
				}
				assertNoSharedMemory(t, "input", reflect.ValueOf(got).Elem(), reflect.ValueOf(input).Elem())
			})
		}
	}
}

// An empty non-nil struct pointer (such as vrf: {}, ipvlan: {}, ethtool: {}, or
// a presence-only struct like vfio: {}) must survive merge and Clone without
// aliasing the input.
func TestMergeKeepsEmptyStructPointersAndDoesNotAlias(t *testing.T) {
	for _, node := range networkConfigStructPtrs {
		want := emptyStructPtrConfig(node)
		for _, tc := range []struct {
			name string
			op   func(input *NetworkConfig) *NetworkConfig
		}{
			{"user only", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(in, &NetworkConfig{}) }},
			{"user only, nil cloud", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(in, nil) }},
			{"cloud only", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(&NetworkConfig{}, in) }},
			{"cloud only, nil user", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(nil, in) }},
			{"clone", func(in *NetworkConfig) *NetworkConfig { return in.Clone() }},
		} {
			t.Run(node.path+"/"+tc.name, func(t *testing.T) {
				input := emptyStructPtrConfig(node)
				got := tc.op(input)
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("(-want +got):\n%s", diff)
				}
				assertNoSharedMemory(t, "input", reflect.ValueOf(got).Elem(), reflect.ValueOf(input).Elem())
			})
		}
	}
}

// With both sides set, every scalar and pointer takes the user value, and slices
// and maps combine. Tests both distinct VRF names (unit replacement) and
// matching VRF names (field merge), with distinct Interface.Type values so
// swapping firstNonEmpty(user.Type, cloud.Type) cannot go undetected.
func TestMergeUserWinsEveryField(t *testing.T) {
	for _, sameVRF := range []bool{false, true} {
		name := "distinct VRF names"
		if sameVRF {
			name = "same VRF name"
		}
		t.Run(name, func(t *testing.T) {
			user := fullConfig("user", 0)
			cloud := fullConfig("cloud", 60)
			want := fullConfig("user", 0)

			user.Interface.Type = InterfaceTypeIPVLAN
			cloud.Interface.Type = InterfaceTypePassthrough
			want.Interface.Type = InterfaceTypeIPVLAN
			if sameVRF {
				user.Interface.VRF.Name = "shared-vrf"
				cloud.Interface.VRF.Name = "shared-vrf"
				want.Interface.VRF.Name = "shared-vrf"
			}

			want.Interface.Addresses = append(append([]string{}, cloud.Interface.Addresses...), user.Interface.Addresses...)
			want.Routes = append(append([]RouteConfig{}, cloud.Routes...), user.Routes...)
			want.Rules = append(append([]RuleConfig{}, cloud.Rules...), user.Rules...)
			want.Neighbors = append(append([]NeighborConfig{}, cloud.Neighbors...), user.Neighbors...)
			want.Ethtool.Features = unionBoolMaps(cloud.Ethtool.Features, user.Ethtool.Features)
			want.Ethtool.PrivateFlags = unionBoolMaps(cloud.Ethtool.PrivateFlags, user.Ethtool.PrivateFlags)

			if diff := cmp.Diff(want, MergeNetworkConfig(user, cloud)); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

// On every map leaf, a conflicting key must take the user value in both
// directions (false over true and true over false, or zero over non-zero),
// preserve disjoint keys from both sides, and share no map with inputs.
func TestMergeMapKeyConflictsUserWins(t *testing.T) {
	for _, l := range networkConfigLeaves {
		probe := &NetworkConfig{}
		mapType := l.get(reflect.ValueOf(probe).Elem()).Type()
		if mapType.Kind() != reflect.Map {
			continue
		}
		t.Run(l.path, func(t *testing.T) {
			for _, userFalseOverTrue := range []bool{true, false} {
				caseName := "user zero overrides cloud non-zero"
				if !userFalseOverTrue {
					caseName = "user non-zero overrides cloud zero"
				}
				t.Run(caseName, func(t *testing.T) {
					user := &NetworkConfig{}
					cloud := &NetworkConfig{}
					want := &NetworkConfig{}

					uMap := reflect.MakeMap(mapType)
					cMap := reflect.MakeMap(mapType)
					wMap := reflect.MakeMap(mapType)

					makeKey := func(label string, n int64) reflect.Value {
						k := reflect.New(mapType.Key()).Elem()
						next := n
						setValue(k, l.path+"["+label+"]", label, &next, false)
						return k
					}
					makeVal := func(label string, n int64, zero bool) reflect.Value {
						v := reflect.New(mapType.Elem()).Elem()
						next := n
						setValue(v, l.path+"["+label+"]", label, &next, zero)
						return v
					}

					sharedKey := makeKey("shared", 1)
					userOnlyKey := makeKey("user-only", 2)
					cloudOnlyKey := makeKey("cloud-only", 3)

					uShared := makeVal("user", 10, userFalseOverTrue)
					cShared := makeVal("cloud", 20, !userFalseOverTrue)
					uOnly := makeVal("uonly", 11, false)
					cOnly := makeVal("conly", 21, false)

					uMap.SetMapIndex(sharedKey, uShared)
					uMap.SetMapIndex(userOnlyKey, uOnly)
					cMap.SetMapIndex(sharedKey, cShared)
					cMap.SetMapIndex(cloudOnlyKey, cOnly)
					wMap.SetMapIndex(sharedKey, uShared)
					wMap.SetMapIndex(userOnlyKey, uOnly)
					wMap.SetMapIndex(cloudOnlyKey, cOnly)

					l.get(reflect.ValueOf(user).Elem()).Set(uMap)
					l.get(reflect.ValueOf(cloud).Elem()).Set(cMap)
					l.get(reflect.ValueOf(want).Elem()).Set(wMap)

					got := MergeNetworkConfig(user, cloud)
					if diff := cmp.Diff(want, got); diff != "" {
						t.Errorf("(-want +got):\n%s", diff)
					}
					assertNoSharedMemory(t, "user", reflect.ValueOf(got).Elem(), reflect.ValueOf(user).Elem())
					assertNoSharedMemory(t, "cloud", reflect.ValueOf(got).Elem(), reflect.ValueOf(cloud).Elem())
				})
			}
		})
	}
}

// Empty non-nil maps must survive merge and Clone as fresh allocations without
// aliasing either input.
func TestMergeEmptyNonNilMapsDoNotAlias(t *testing.T) {
	for _, l := range networkConfigLeaves {
		probe := &NetworkConfig{}
		mapType := l.get(reflect.ValueOf(probe).Elem()).Type()
		if mapType.Kind() != reflect.Map {
			continue
		}
		makeEmpty := func() *NetworkConfig {
			cfg := &NetworkConfig{}
			l.get(reflect.ValueOf(cfg).Elem()).Set(reflect.MakeMap(mapType))
			return cfg
		}
		for _, tc := range []struct {
			name string
			op   func(in *NetworkConfig) *NetworkConfig
		}{
			{"user only", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(in, &NetworkConfig{}) }},
			{"cloud only", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(&NetworkConfig{}, in) }},
			{"both", func(in *NetworkConfig) *NetworkConfig { return MergeNetworkConfig(in, makeEmpty()) }},
			{"clone", func(in *NetworkConfig) *NetworkConfig { return in.Clone() }},
		} {
			t.Run(l.path+"/"+tc.name, func(t *testing.T) {
				input := makeEmpty()
				got := tc.op(input)
				if diff := cmp.Diff(makeEmpty(), got); diff != "" {
					t.Errorf("(-want +got):\n%s", diff)
				}
				assertNoSharedMemory(t, "input", reflect.ValueOf(got).Elem(), reflect.ValueOf(input).Elem())
			})
		}
	}
}

func unionBoolMaps(first, second map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range first {
		out[k] = v
	}
	for k, v := range second {
		out[k] = v
	}
	return out
}

// A user pointer wins even when it points at a zero value (false, 0, ""), for
// every pointer field at any depth. This is the bug the PR fixes.
func TestMergeUserZeroPointerWinsEveryField(t *testing.T) {
	for _, distinctVRFName := range []bool{false, true} {
		name := "same or empty VRF name"
		if distinctVRFName {
			name = "distinct VRF name"
		}
		t.Run(name, func(t *testing.T) {
			user := zeroConfig()
			if distinctVRFName {
				user.Interface.VRF.Name = "user-vrf"
			}
			cloud := fullConfig("cloud", 60)
			merged := MergeNetworkConfig(user, cloud)
			assertZeroPointersKept(t, "", reflect.ValueOf(merged).Elem(), reflect.ValueOf(user).Elem())
		})
	}
}

func assertZeroPointersKept(t *testing.T, path string, merged, user reflect.Value) {
	t.Helper()
	switch merged.Kind() {
	case reflect.Struct:
		for i := 0; i < merged.NumField(); i++ {
			field := merged.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			assertZeroPointersKept(t, path+"."+field.Name, merged.Field(i), user.Field(i))
		}
	case reflect.Pointer:
		if user.IsNil() {
			return
		}
		if merged.IsNil() {
			t.Errorf("%s: user set it, merged has nil", path)
			return
		}
		if merged.Elem().Kind() == reflect.Struct {
			assertZeroPointersKept(t, path, merged.Elem(), user.Elem())
			return
		}
		if !merged.Elem().IsZero() {
			t.Errorf("%s: user set a zero value, merged has %v", path, merged.Elem().Interface())
		}
	}
}

// hasReferenceType reports whether t contains any pointer, slice, map, or
// interface anywhere in its structure.
func hasReferenceType(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	case reflect.Array:
		return hasReferenceType(t.Elem())
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if hasReferenceType(t.Field(i).Type) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// slicesOverlap reports whether two slices with backing arrays share any part
// of their capacity (including zero-length views into the same backing array).
func slicesOverlap(a, b reflect.Value) bool {
	elemSize := a.Type().Elem().Size()
	if elemSize == 0 || a.Cap() == 0 || b.Cap() == 0 {
		return false
	}
	aStart := a.Pointer()
	aEnd := aStart + uintptr(a.Cap())*elemSize
	bStart := b.Pointer()
	bEnd := bStart + uintptr(b.Cap())*elemSize
	return aStart < bEnd && bStart < aEnd
}

// findSharedMemory walks a and b and reports any pointer, overlapping slice
// backing array, or map shared between them, including inside slice elements
// and map values. Because slice merging appends and deduplicates entries, every
// element/value of a is checked against every element/value of b when the
// element type can hold references.
func findSharedMemory(path string, a, b reflect.Value) []string {
	if !a.IsValid() || !b.IsValid() {
		return nil
	}
	if a.Type() != b.Type() {
		return []string{fmt.Sprintf("%s: type mismatch %v vs %v", path, a.Type(), b.Type())}
	}
	var findings []string
	switch a.Kind() {
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			field := a.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			findings = append(findings, findSharedMemory(path+"."+field.Name, a.Field(i), b.Field(i))...)
		}
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		if a.Elem().Type().Size() > 0 && a.Pointer() == b.Pointer() {
			return []string{fmt.Sprintf("%s shares memory with the input", path)}
		}
		findings = append(findings, findSharedMemory(path, a.Elem(), b.Elem())...)
	case reflect.Map:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			return []string{fmt.Sprintf("%s shares memory with the input", path)}
		}
		if hasReferenceType(a.Type().Elem()) {
			iterA := a.MapRange()
			for iterA.Next() {
				iterB := b.MapRange()
				for iterB.Next() {
					findings = append(findings, findSharedMemory(fmt.Sprintf("%s[%v]", path, iterA.Key().Interface()), iterA.Value(), iterB.Value())...)
				}
			}
		}
	case reflect.Slice:
		if slicesOverlap(a, b) {
			findings = append(findings, fmt.Sprintf("%s shares its backing array with the input", path))
		}
		if a.Len() > 0 && b.Len() > 0 && hasReferenceType(a.Type().Elem()) {
			for i := 0; i < a.Len(); i++ {
				for j := 0; j < b.Len(); j++ {
					findings = append(findings, findSharedMemory(fmt.Sprintf("%s[%d]", path, i), a.Index(i), b.Index(j))...)
				}
			}
		}
	case reflect.Array:
		if hasReferenceType(a.Type().Elem()) {
			for i := 0; i < a.Len(); i++ {
				for j := 0; j < b.Len(); j++ {
					findings = append(findings, findSharedMemory(fmt.Sprintf("%s[%d]", path, i), a.Index(i), b.Index(j))...)
				}
			}
		}
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		if a.Elem().Type() == b.Elem().Type() {
			findings = append(findings, findSharedMemory(path, a.Elem(), b.Elem())...)
		}
	}
	return findings
}

// assertNoSharedMemory walks two values in parallel and fails on any pointer,
// slice backing array, or map that both share. Whole-struct equality cannot see
// aliasing, so this check is separate.
func assertNoSharedMemory(t *testing.T, path string, a, b reflect.Value) {
	t.Helper()
	for _, finding := range findSharedMemory(path, a, b) {
		t.Error(finding)
	}
}

// The result must not share memory with either input, including when the other
// input is nil, because a merge can take a shortcut on that path.
func TestMergeSharesNoMemoryWithInputs(t *testing.T) {
	for _, tc := range []struct {
		name        string
		user, cloud *NetworkConfig
	}{
		{"user only", fullConfig("x", 0), &NetworkConfig{}},
		{"user only, nil cloud", fullConfig("x", 0), nil},
		{"cloud only", &NetworkConfig{}, fullConfig("x", 0)},
		{"cloud only, nil user", nil, fullConfig("x", 0)},
		{"both", fullConfig("user", 0), fullConfig("cloud", 60)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged := MergeNetworkConfig(tc.user, tc.cloud)
			for name, input := range map[string]*NetworkConfig{"user": tc.user, "cloud": tc.cloud} {
				if input == nil {
					continue
				}
				assertNoSharedMemory(t, name, reflect.ValueOf(merged).Elem(), reflect.ValueOf(input).Elem())
			}
		})
	}
}

// Clone must preserve every field (including duplicate slice entries across all
// slice fields that MergeNetworkConfig deduplicates) and share no memory with
// the source.
func TestClonePreservesAndIsolatesAllFields(t *testing.T) {
	var nilCfg *NetworkConfig
	if got := nilCfg.Clone(); got != nil {
		t.Fatalf("nil.Clone() = %#v, want nil", got)
	}

	duplicateAllSlices := func(cfg *NetworkConfig) {
		root := reflect.ValueOf(cfg).Elem()
		for _, l := range networkConfigLeaves {
			v := l.get(root)
			if v.Kind() == reflect.Slice && v.Len() > 0 {
				v.Set(reflect.Append(v, v.Index(0)))
			}
		}
	}

	input := fullConfig("x", 0)
	duplicateAllSlices(input)
	want := fullConfig("x", 0)
	duplicateAllSlices(want)

	cloned := input.Clone()
	if diff := cmp.Diff(want, cloned); diff != "" {
		t.Errorf("Clone() mismatch (-want +got):\n%s", diff)
	}
	assertNoSharedMemory(t, "input", reflect.ValueOf(cloned).Elem(), reflect.ValueOf(input).Elem())
}

// Verify the reflection detectors themselves catch reference leaks inside slice
// elements and map values, overlapping sub-slices (including spare capacity),
// and empty/zero-sized struct pointers.
func TestReflectionEnforcementDetectors(t *testing.T) {
	type routeWithPtr struct {
		Metric *int
	}
	type container struct {
		Routes []routeWithPtr
		ByKey  map[string]*int
		Slice  []int
	}

	sharedMetric := ptr.To(42)
	t.Run("catches shared pointer inside slice element at shifted index", func(t *testing.T) {
		in := container{Routes: []routeWithPtr{{Metric: sharedMetric}}}
		out := container{Routes: []routeWithPtr{{Metric: ptr.To(1)}, {Metric: sharedMetric}}}
		if findings := findSharedMemory("input", reflect.ValueOf(out), reflect.ValueOf(in)); len(findings) == 0 {
			t.Fatal("expected findSharedMemory to detect shared pointer inside slice element")
		}
	})

	t.Run("catches shared pointer inside map value", func(t *testing.T) {
		in := container{ByKey: map[string]*int{"a": sharedMetric}}
		out := container{ByKey: map[string]*int{"b": sharedMetric}}
		if findings := findSharedMemory("input", reflect.ValueOf(out), reflect.ValueOf(in)); len(findings) == 0 {
			t.Fatal("expected findSharedMemory to detect shared pointer inside map value")
		}
	})

	t.Run("catches overlapping sub-slice backing array", func(t *testing.T) {
		backing := []int{1, 2, 3}
		in := container{Slice: backing}
		out := container{Slice: backing[1:]}
		if findings := findSharedMemory("input", reflect.ValueOf(out), reflect.ValueOf(in)); len(findings) == 0 {
			t.Fatal("expected findSharedMemory to detect overlapping sub-slice")
		}
	})

	t.Run("catches shared spare capacity between zero-length and non-empty slices", func(t *testing.T) {
		backing := make([]int, 1, 4)
		backing[0] = 7
		in := container{Slice: backing}
		out := container{Slice: backing[:0]}
		if findings := findSharedMemory("input", reflect.ValueOf(out), reflect.ValueOf(in)); len(findings) == 0 {
			t.Fatal("expected findSharedMemory to detect zero-length slice sharing backing capacity")
		}
	})

	t.Run("discovers empty and zero-sized struct pointers as leaf and structPtrNode", func(t *testing.T) {
		type emptyMarker struct{}
		type zeroSizedWrapper struct {
			_ [0]byte
		}
		type withEmpty struct {
			Marker  *emptyMarker
			Wrapper *zeroSizedWrapper
		}
		leaves := collectLeaves(reflect.TypeOf(withEmpty{}), "", func(r reflect.Value) reflect.Value { return r })
		if len(leaves) != 2 || leaves[0].path != "Marker" || leaves[1].path != "Wrapper" {
			t.Fatalf("collectLeaves() = %+v, want Marker and Wrapper", leaves)
		}
		ptrs := collectStructPointers(reflect.TypeOf(withEmpty{}), "", func(r reflect.Value) reflect.Value { return r })
		if len(ptrs) != 2 || ptrs[0].path != "Marker" || ptrs[1].path != "Wrapper" {
			t.Fatalf("collectStructPointers() = %+v, want Marker and Wrapper", ptrs)
		}
	})
}
