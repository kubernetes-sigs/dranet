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
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// These tests walk the NetworkConfig type with reflection, so a new field is
// covered without a field list. Every leaf gets a value that names its own path,
// so a merge that reads the wrong field shows up as a difference.

// leaf is one settable value: a scalar, a pointer to a scalar, a slice, or a map.
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
		if t.Elem().Kind() != reflect.Struct {
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

var networkConfigLeaves = collectLeaves(reflect.TypeOf(NetworkConfig{}), "", func(root reflect.Value) reflect.Value { return root })

// setValue writes a value into v. Strings carry marker and path, numbers carry n,
// bools are true. With zero set, pointers are allocated but every value is zero.
func setValue(v reflect.Value, path, marker string, n int64, zero bool) {
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		setValue(v.Elem(), path, marker, n, zero)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			setValue(v.Field(i), path+"."+v.Type().Field(i).Name, marker, n, zero)
		}
	case reflect.String:
		if !zero {
			v.SetString(marker + ":" + path)
		}
	case reflect.Bool:
		v.SetBool(!zero)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(n))
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		setValue(elem, path, marker, n, zero)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), elem))
	case reflect.Map:
		key := reflect.New(v.Type().Key()).Elem()
		setValue(key, path+"[key]", marker, n, zero)
		val := reflect.New(v.Type().Elem()).Elem()
		setValue(val, path+"[value]", marker, n, zero)
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(key, val)
		v.Set(m)
	default:
		panic("setValue: unsupported kind " + v.Kind().String() + " at " + path)
	}
}

// fullConfig sets every leaf. Numbers are offset + leaf ordinal, so two configs
// with different offsets share no number. Keep offset + leaf count under 256
// because RouteConfig.Scope is a uint8.
func fullConfig(marker string, offset int64) *NetworkConfig {
	config := &NetworkConfig{}
	root := reflect.ValueOf(config).Elem()
	for i, l := range networkConfigLeaves {
		setValue(l.get(root), l.path, marker, offset+int64(i)+1, false)
	}
	// The merge drops IPVlan unless the type is IPVLAN.
	config.Interface.Type = InterfaceTypeIPVLAN
	return config
}

// zeroConfig allocates every pointer but leaves every value zero.
func zeroConfig() *NetworkConfig {
	config := &NetworkConfig{}
	root := reflect.ValueOf(config).Elem()
	for _, l := range networkConfigLeaves {
		setValue(l.get(root), l.path, "", 0, true)
	}
	config.Interface.Type = InterfaceTypeIPVLAN
	return config
}

// singleLeafConfig sets one leaf only.
func singleLeafConfig(l leaf, n int64) *NetworkConfig {
	config := &NetworkConfig{}
	setValue(l.get(reflect.ValueOf(config).Elem()), l.path, "x", n, false)
	if strings.HasPrefix(l.path, "Interface.IPVlan") {
		config.Interface.Type = InterfaceTypeIPVLAN
	}
	return config
}

// A value set on one side only must survive the merge unchanged, for every field.
func TestMergeKeepsEveryFieldFromEitherSide(t *testing.T) {
	full := fullConfig("x", 0)
	for _, tc := range []struct {
		name        string
		user, cloud *NetworkConfig
	}{
		{"user only", full, &NetworkConfig{}},
		{"cloud only", &NetworkConfig{}, full},
		{"user only, nil cloud", full, nil},
		{"cloud only, nil user", nil, full},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(full, MergeNetworkConfig(tc.user, tc.cloud)); diff != "" {
				t.Errorf("merge lost or changed a field (-want +got):\n%s", diff)
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

// With both sides set, every scalar and pointer takes the user value, and slices
// and maps combine.
func TestMergeUserWinsEveryField(t *testing.T) {
	user := fullConfig("user", 0)
	cloud := fullConfig("cloud", 100)
	want := fullConfig("user", 0)
	want.Interface.Addresses = append(append([]string{}, cloud.Interface.Addresses...), user.Interface.Addresses...)
	want.Routes = append(append([]RouteConfig{}, cloud.Routes...), user.Routes...)
	want.Rules = append(append([]RuleConfig{}, cloud.Rules...), user.Rules...)
	want.Neighbors = append(append([]NeighborConfig{}, cloud.Neighbors...), user.Neighbors...)
	want.Ethtool.Features = unionBoolMaps(cloud.Ethtool.Features, user.Ethtool.Features)
	want.Ethtool.PrivateFlags = unionBoolMaps(cloud.Ethtool.PrivateFlags, user.Ethtool.PrivateFlags)

	if diff := cmp.Diff(want, MergeNetworkConfig(user, cloud)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
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
	user := zeroConfig()
	cloud := fullConfig("cloud", 100)
	merged := MergeNetworkConfig(user, cloud)
	assertZeroPointersKept(t, "", reflect.ValueOf(merged).Elem(), reflect.ValueOf(user).Elem())
}

func assertZeroPointersKept(t *testing.T, path string, merged, user reflect.Value) {
	t.Helper()
	switch merged.Kind() {
	case reflect.Struct:
		for i := 0; i < merged.NumField(); i++ {
			assertZeroPointersKept(t, path+"."+merged.Type().Field(i).Name, merged.Field(i), user.Field(i))
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

// assertNoSharedMemory walks two values in parallel and fails on any pointer,
// slice backing array, or map that both share. Whole-struct equality cannot see
// aliasing, so this check is separate.
func assertNoSharedMemory(t *testing.T, path string, a, b reflect.Value) {
	t.Helper()
	switch a.Kind() {
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			assertNoSharedMemory(t, path+"."+a.Type().Field(i).Name, a.Field(i), b.Field(i))
		}
	case reflect.Pointer, reflect.Map:
		if a.IsNil() || b.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s shares memory with the input", path)
			return
		}
		if a.Kind() == reflect.Pointer {
			assertNoSharedMemory(t, path, a.Elem(), b.Elem())
		}
	case reflect.Slice:
		if a.Len() == 0 || b.Len() == 0 {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s shares its backing array with the input", path)
		}
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
		{"both", fullConfig("user", 0), fullConfig("cloud", 100)},
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
