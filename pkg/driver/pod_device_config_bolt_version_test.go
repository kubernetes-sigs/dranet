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
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/pkg/apis"
)

// fullDeviceConfig returns a DeviceConfig with every field populated, so the
// roundtrip and golden tests exercise the complete checkpoint wire format.
// TestDeviceConfigFixturePopulated fails when a new field is added but not
// set here, forcing the golden to be updated together with the schema.
func fullDeviceConfig() DeviceConfig {
	return DeviceConfig{
		Claim: types.NamespacedName{Namespace: "default", Name: "claim-1"},
		DeviceSnapshot: &resourceapi.Device{
			Name: "dev0",
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"dra.net/mac": {StringValue: ptr.To("aa:bb:cc:dd:ee:ff")},
			},
		},
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{
				Name:         "eth1",
				Addresses:    []string{"192.168.1.10/24"},
				HardwareAddr: ptr.To("aa:bb:cc:dd:ee:ff"),
			},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Profile: "hpc",
			Interface: apis.InterfaceConfig{
				Name:                "net0",
				Type:                apis.InterfaceTypeIPVLAN,
				Addressing:          apis.AddressingModeDHCP,
				Addresses:           []string{"10.0.0.5/24"},
				DHCP:                ptr.To(true),
				MTU:                 ptr.To(int32(9000)),
				HardwareAddr:        ptr.To("02:00:00:00:00:01"),
				GSOMaxSize:          ptr.To(int32(65536)),
				GROMaxSize:          ptr.To(int32(65536)),
				GSOIPv4MaxSize:      ptr.To(int32(65536)),
				GROIPv4MaxSize:      ptr.To(int32(65536)),
				DisableEBPFPrograms: ptr.To(true),
				Forwarding:          ptr.To(true),
				ARPIgnore:           ptr.To(int32(1)),
				ARPAnnounce:         ptr.To(int32(2)),
				VRF:                 &apis.VRFConfig{Name: "vrf0", Table: ptr.To(100)},
				IPVlan:              &apis.IPVlanConfig{Mode: apis.IPVlanModeL2, Flag: apis.IPVlanFlagBridge},
			},
			Routes: []apis.RouteConfig{{
				Destination: "0.0.0.0/0",
				Gateway:     "10.0.0.1",
				Source:      "10.0.0.5",
				Scope:       253,
				Table:       100,
			}},
			Rules: []apis.RuleConfig{{
				Priority:    1000,
				Source:      "10.0.0.5/32",
				Destination: "10.1.0.0/16",
				Table:       100,
			}},
			Neighbors: []apis.NeighborConfig{{
				Destination:  "10.0.0.1",
				HardwareAddr: "02:00:00:00:00:02",
			}},
			Ethtool: &apis.EthtoolConfig{
				Features:     map[string]bool{"tcp-segmentation-offload": true},
				PrivateFlags: map[string]bool{"my-flag": false},
			},
		},
		RDMADevice: RDMAConfig{
			LinkDev: "mlx5_0",
			DevChars: []LinuxDevice{{
				Path:     "/dev/infiniband/uverbs0",
				Type:     "c",
				Major:    231,
				Minor:    192,
				FileMode: 0666,
				UID:      1000,
				GID:      1000,
			}},
		},
	}
}

// goldenDeviceConfigJSON is the checkpoint wire representation of
// fullDeviceConfig(). If TestDeviceConfigWireFormatGolden fails, the
// checkpoint format changed: either make the change additive, or bump
// checkpointSchemaVersion and add a checkpointMigrations entry, then
// regenerate this golden.
const goldenDeviceConfigJSON = `{"claim":{"Namespace":"default","Name":"claim-1"},"deviceSnapshot":{"name":"dev0","attributes":{"dra.net/mac":{"string":"aa:bb:cc:dd:ee:ff"}}},"networkInterfaceConfigInHost":{"interface":{"name":"eth1","addresses":["192.168.1.10/24"],"hardwareAddr":"aa:bb:cc:dd:ee:ff"}},"networkInterfaceConfigInPod":{"profile":"hpc","interface":{"name":"net0","type":"IPVLAN","addressing":"DHCP","addresses":["10.0.0.5/24"],"dhcp":true,"mtu":9000,"hardwareAddr":"02:00:00:00:00:01","gsoMaxSize":65536,"groMaxSize":65536,"gsoIPv4MaxSize":65536,"groIPv4MaxSize":65536,"disableEbpfPrograms":true,"forwarding":true,"arpIgnore":1,"arpAnnounce":2,"vrf":{"name":"vrf0","table":100},"ipvlan":{"mode":"L2","flag":"Bridge"}},"routes":[{"destination":"0.0.0.0/0","gateway":"10.0.0.1","source":"10.0.0.5","scope":253,"table":100}],"rules":[{"priority":1000,"source":"10.0.0.5/32","destination":"10.1.0.0/16","table":100}],"neighbors":[{"destination":"10.0.0.1","hardwareAddr":"02:00:00:00:00:02"}],"ethtool":{"features":{"tcp-segmentation-offload":true},"privateFlags":{"my-flag":false}}},"rdmaDevice":{"linkDev":"mlx5_0","devChars":[{"path":"/dev/infiniband/uverbs0","type":"c","major":231,"minor":192,"fileMode":438,"uid":1000,"gid":1000}]}}`

func openRawBolt(t *testing.T, path string) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("open raw bolt db: %v", err)
	}
	return db
}

func readSchemaVersion(t *testing.T, path string) string {
	t.Helper()
	db := openRawBolt(t, path)
	defer db.Close()
	var version string
	err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return fmt.Errorf("meta bucket missing")
		}
		version = string(meta.Get(schemaVersionKey))
		return nil
	})
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	return version
}

func stampSchemaVersion(t *testing.T, path, version string) {
	t.Helper()
	db := openRawBolt(t, path)
	defer db.Close()
	err := db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		return meta.Put(schemaVersionKey, []byte(version))
	})
	if err != nil {
		t.Fatalf("stamp schema version: %v", err)
	}
}

// seedPreVersioningDB writes a device config using the pre-versioning layout:
// pod_configs bucket only, no meta bucket.
func seedPreVersioningDB(t *testing.T, path, podUID, deviceName string, cfg DeviceConfig) {
	t.Helper()
	db := openRawBolt(t, path)
	defer db.Close()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal device config: %v", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		root, err := tx.CreateBucketIfNotExists(podConfigsBucket)
		if err != nil {
			return err
		}
		podBucket, err := root.CreateBucketIfNotExists([]byte(podUID))
		if err != nil {
			return err
		}
		devBucket, err := podBucket.CreateBucketIfNotExists(deviceConfigsKey)
		if err != nil {
			return err
		}
		return devBucket.Put([]byte(deviceName), data)
	})
	if err != nil {
		t.Fatalf("seed pre-versioning db: %v", err)
	}
}

// A field added to the checkpointed types but not to fullDeviceConfig() would
// silently escape the roundtrip and golden coverage below.
func TestDeviceConfigFixturePopulated(t *testing.T) {
	cfg := fullDeviceConfig()
	for name, v := range map[string]reflect.Value{
		"DeviceConfig":    reflect.ValueOf(cfg),
		"NetworkConfig":   reflect.ValueOf(cfg.NetworkInterfaceConfigInPod),
		"InterfaceConfig": reflect.ValueOf(cfg.NetworkInterfaceConfigInPod.Interface),
		"RDMAConfig":      reflect.ValueOf(cfg.RDMADevice),
		"LinuxDevice":     reflect.ValueOf(cfg.RDMADevice.DevChars[0]),
		"RouteConfig":     reflect.ValueOf(cfg.NetworkInterfaceConfigInPod.Routes[0]),
		"RuleConfig":      reflect.ValueOf(cfg.NetworkInterfaceConfigInPod.Rules[0]),
		"NeighborConfig":  reflect.ValueOf(cfg.NetworkInterfaceConfigInPod.Neighbors[0]),
		"EthtoolConfig":   reflect.ValueOf(*cfg.NetworkInterfaceConfigInPod.Ethtool),
	} {
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			if v.Field(i).IsZero() {
				t.Errorf("%s.%s is not populated by fullDeviceConfig(): set it and regenerate goldenDeviceConfigJSON", name, field.Name)
			}
		}
	}
}

// A checkpointed DeviceConfig must survive Store → GetOrCreate and a full
// close/reopen cycle without losing or altering any field.
func TestDeviceConfigCheckpointRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod_configs.db")
	want := fullDeviceConfig()

	cp, err := newBoltCheckpointer(path)
	if err != nil {
		t.Fatalf("newBoltCheckpointer: %v", err)
	}
	if err := cp.Store("pod-1", "dev-1", want); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := cp.GetOrCreate()
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if !reflect.DeepEqual(got["pod-1"]["dev-1"], want) {
		t.Errorf("roundtrip mismatch:\n got %#v\nwant %#v", got["pod-1"]["dev-1"], want)
	}
	if err := cp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// daemon restart
	cp2, err := newBoltCheckpointer(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer cp2.Close()
	got2, err := cp2.GetOrCreate()
	if err != nil {
		t.Fatalf("GetOrCreate after reopen: %v", err)
	}
	if !reflect.DeepEqual(got2["pod-1"]["dev-1"], want) {
		t.Errorf("roundtrip after reopen mismatch:\n got %#v\nwant %#v", got2["pod-1"]["dev-1"], want)
	}
}

// The golden pins the exact bytes on disk in both directions: struct→JSON
// catches renamed/retagged fields, JSON→struct catches asymmetric decoding.
func TestDeviceConfigWireFormatGolden(t *testing.T) {
	want := fullDeviceConfig()

	got, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != goldenDeviceConfigJSON {
		t.Errorf("checkpoint wire format changed; if intentional bump checkpointSchemaVersion, add a migration and update the golden.\n got: %s\ngolden: %s", got, goldenDeviceConfigJSON)
	}

	var decoded DeviceConfig
	if err := json.Unmarshal([]byte(goldenDeviceConfigJSON), &decoded); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Errorf("golden does not decode back to the fixture:\n got %#v\nwant %#v", decoded, want)
	}
}

// Unknown JSON fields are dropped silently: this is what a daemon downgrade
// does to state written by a newer version. Documented here so a change in
// behaviour (e.g. strict decoding) is a conscious decision.
func TestDeviceConfigUnknownFieldsIgnored(t *testing.T) {
	var generic map[string]any
	if err := json.Unmarshal([]byte(goldenDeviceConfigJSON), &generic); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	generic["fieldFromANewerVersion"] = map[string]any{"x": 1}
	data, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded DeviceConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("a newer checkpoint must still load: %v", err)
	}
	if !reflect.DeepEqual(decoded, fullDeviceConfig()) {
		t.Errorf("known fields corrupted by unknown sibling:\n got %#v", decoded)
	}
}

func TestCheckpointSchemaFreshDBStamped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod_configs.db")
	cp, err := newBoltCheckpointer(path)
	if err != nil {
		t.Fatalf("newBoltCheckpointer: %v", err)
	}
	cp.Close()
	if got, want := readSchemaVersion(t, path), strconv.Itoa(checkpointSchemaVersion); got != want {
		t.Errorf("schemaVersion = %q, want %q", got, want)
	}
}

// A database written before versioning existed (no meta bucket) is version 1:
// it must load, keep its data intact and get stamped.
func TestCheckpointSchemaPreVersioningDBUpgraded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod_configs.db")
	want := fullDeviceConfig()
	seedPreVersioningDB(t, path, "pod-1", "dev-1", want)

	cp, err := newBoltCheckpointer(path)
	if err != nil {
		t.Fatalf("newBoltCheckpointer on pre-versioning db: %v", err)
	}
	got, err := cp.GetOrCreate()
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if !reflect.DeepEqual(got["pod-1"]["dev-1"], want) {
		t.Errorf("pre-versioning data mismatch:\n got %#v\nwant %#v", got["pod-1"]["dev-1"], want)
	}
	cp.Close()
	if got, want := readSchemaVersion(t, path), strconv.Itoa(checkpointSchemaVersion); got != want {
		t.Errorf("schemaVersion = %q, want %q", got, want)
	}
}

// State written by a newer daemon cannot be rebuilt if dropped, so a
// downgrade must refuse to open the database rather than lose it.
func TestCheckpointSchemaNewerVersionRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod_configs.db")
	future := strconv.Itoa(checkpointSchemaVersion + 1)
	stampSchemaVersion(t, path, future)

	if _, err := newBoltCheckpointer(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("expected refusal for newer schema, got err=%v", err)
	}
	// the refusal must not modify the database
	if got := readSchemaVersion(t, path); got != future {
		t.Errorf("schemaVersion = %q, want untouched %q", got, future)
	}
}

func TestCheckpointSchemaMalformedVersionRefused(t *testing.T) {
	for _, version := range []string{"banana", "0", "-1", ""} {
		t.Run(fmt.Sprintf("version=%q", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pod_configs.db")
			stampSchemaVersion(t, path, version)
			if _, err := newBoltCheckpointer(path); err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("expected malformed version error, got err=%v", err)
			}
		})
	}
}

func TestMigrateSchemaChainRunsInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod_configs.db")
	db := openRawBolt(t, path)

	var ran []int
	migrations := map[int]func(tx *bolt.Tx) error{
		1: func(*bolt.Tx) error { ran = append(ran, 1); return nil },
		2: func(*bolt.Tx) error { ran = append(ran, 2); return nil },
	}
	if err := db.Update(func(tx *bolt.Tx) error { return migrateSchema(tx, 3, migrations) }); err != nil {
		t.Fatalf("migrateSchema: %v", err)
	}
	db.Close()

	if !reflect.DeepEqual(ran, []int{1, 2}) {
		t.Errorf("migrations ran = %v, want [1 2]", ran)
	}
	if got := readSchemaVersion(t, path); got != "3" {
		t.Errorf("schemaVersion = %q, want %q", got, "3")
	}
}

func TestMigrateSchemaMissingMigrationFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod_configs.db")
	db := openRawBolt(t, path)
	defer db.Close()

	migrations := map[int]func(tx *bolt.Tx) error{
		1: func(*bolt.Tx) error { return nil },
		// no migration from 2 to 3
	}
	err := db.Update(func(tx *bolt.Tx) error { return migrateSchema(tx, 3, migrations) })
	if err == nil || !strings.Contains(err.Error(), "no migration") {
		t.Fatalf("expected missing migration error, got err=%v", err)
	}
}

// A migration failure must roll back everything it did — data mutations and
// version stamp alike — leaving the previous layout loadable.
func TestMigrateSchemaFailedMigrationRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod_configs.db")
	want := fullDeviceConfig()
	seedPreVersioningDB(t, path, "pod-1", "dev-1", want)

	boom := errors.New("boom")
	migrations := map[int]func(tx *bolt.Tx) error{
		1: func(tx *bolt.Tx) error {
			// destroy the data, then fail: nothing may survive the rollback
			if err := tx.DeleteBucket(podConfigsBucket); err != nil {
				return err
			}
			return boom
		},
	}
	db := openRawBolt(t, path)
	err := db.Update(func(tx *bolt.Tx) error { return migrateSchema(tx, 2, migrations) })
	if !errors.Is(err, boom) {
		t.Fatalf("expected migration failure, got err=%v", err)
	}
	db.Close()

	// no version was stamped ...
	db = openRawBolt(t, path)
	err = db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(metaBucket) != nil {
			return fmt.Errorf("meta bucket exists after rollback")
		}
		return nil
	})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	// ... and the pre-migration data is fully intact
	cp, err := newBoltCheckpointer(path)
	if err != nil {
		t.Fatalf("reopen after rollback: %v", err)
	}
	defer cp.Close()
	got, err := cp.GetOrCreate()
	if err != nil {
		t.Fatalf("GetOrCreate after rollback: %v", err)
	}
	if !reflect.DeepEqual(got["pod-1"]["dev-1"], want) {
		t.Errorf("data lost by rolled-back migration:\n got %#v\nwant %#v", got["pod-1"]["dev-1"], want)
	}
}

func readRawDeviceConfig(t *testing.T, path, podUID, deviceName string) []byte {
	t.Helper()
	db := openRawBolt(t, path)
	defer db.Close()
	var data []byte
	err := db.View(func(tx *bolt.Tx) error {
		devBucket := tx.Bucket(podConfigsBucket).Bucket([]byte(podUID)).Bucket(deviceConfigsKey)
		data = append(data, devBucket.Get([]byte(deviceName))...)
		return nil
	})
	if err != nil {
		t.Fatalf("read raw device config: %v", err)
	}
	return data
}

func writeRawDeviceConfig(t *testing.T, path, podUID, deviceName string, data []byte) {
	t.Helper()
	db := openRawBolt(t, path)
	defer db.Close()
	err := db.Update(func(tx *bolt.Tx) error {
		devBucket := tx.Bucket(podConfigsBucket).Bucket([]byte(podUID)).Bucket(deviceConfigsKey)
		return devBucket.Put([]byte(deviceName), data)
	})
	if err != nil {
		t.Fatalf("write raw device config: %v", err)
	}
}

// A downgrade within the same schema version must not destroy state written
// by the newer daemon: reads (GetOrCreate) are non-destructive, so unknown
// fields stay on disk for a later re-upgrade. Only rewriting the same entry
// (Store, i.e. a re-prepare of that claim) drops them — that is the exact
// loss boundary of a same-schema rollback.
func TestCheckpointDowngradeSameSchemaKeepsNewerFieldsOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod_configs.db")
	cp, err := newBoltCheckpointer(path)
	if err != nil {
		t.Fatalf("newBoltCheckpointer: %v", err)
	}
	if err := cp.Store("pod-1", "dev-1", fullDeviceConfig()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	cp.Close()

	// a newer daemon, same schema version, added an optional field
	var generic map[string]any
	if err := json.Unmarshal(readRawDeviceConfig(t, path, "pod-1", "dev-1"), &generic); err != nil {
		t.Fatalf("unmarshal raw config: %v", err)
	}
	generic["fieldFromANewerVersion"] = map[string]any{"x": 1}
	newer, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeRawDeviceConfig(t, path, "pod-1", "dev-1", newer)

	// the rolled-back daemon reads the checkpoint...
	cp, err = newBoltCheckpointer(path)
	if err != nil {
		t.Fatalf("reopen as rolled-back daemon: %v", err)
	}
	got, err := cp.GetOrCreate()
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if !reflect.DeepEqual(got["pod-1"]["dev-1"], fullDeviceConfig()) {
		t.Errorf("known fields corrupted by newer sibling:\n got %#v", got["pod-1"]["dev-1"])
	}
	cp.Close()

	// ... and the newer field is still on disk for a later re-upgrade
	if !strings.Contains(string(readRawDeviceConfig(t, path, "pod-1", "dev-1")), "fieldFromANewerVersion") {
		t.Error("newer field lost from disk by a read-only downgrade")
	}

	// only rewriting the entry drops it
	cp, err = newBoltCheckpointer(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := cp.Store("pod-1", "dev-1", fullDeviceConfig()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	cp.Close()
	if strings.Contains(string(readRawDeviceConfig(t, path, "pod-1", "dev-1")), "fieldFromANewerVersion") {
		t.Error("rewrite by the rolled-back daemon should drop unknown fields")
	}
}

// The recovery path from a downgrade refusal is rolling forward again: the
// newer daemon must open the refused database cleanly with all data intact.
func TestCheckpointRollForwardAfterDowngradeRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod_configs.db")
	want := fullDeviceConfig()

	// a newer daemon migrated the db to schema version+1 and wrote state
	futureVersion := checkpointSchemaVersion + 1
	futureMigrations := map[int]func(tx *bolt.Tx) error{
		checkpointSchemaVersion: func(*bolt.Tx) error { return nil },
	}
	db := openRawBolt(t, path)
	err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(podConfigsBucket); err != nil {
			return err
		}
		return migrateSchema(tx, futureVersion, futureMigrations)
	})
	if err != nil {
		t.Fatalf("simulate newer daemon migration: %v", err)
	}
	futureCP := &boltCheckpointer{db: db}
	if err := futureCP.Store("pod-1", "dev-1", want); err != nil {
		t.Fatalf("Store as newer daemon: %v", err)
	}
	futureCP.Close()

	// the rolled-back daemon refuses to open it, repeatedly (CrashLoopBackOff)
	for i := 0; i < 2; i++ {
		if _, err := newBoltCheckpointer(path); err == nil {
			t.Fatal("rolled-back daemon must refuse a newer schema")
		}
	}

	// rolling forward again: the newer daemon reopens it, no migration to run,
	// data intact
	db = openRawBolt(t, path)
	err = db.Update(func(tx *bolt.Tx) error { return migrateSchema(tx, futureVersion, futureMigrations) })
	if err != nil {
		t.Fatalf("roll-forward reopen: %v", err)
	}
	futureCP = &boltCheckpointer{db: db}
	defer futureCP.Close()
	got, err := futureCP.GetOrCreate()
	if err != nil {
		t.Fatalf("GetOrCreate after roll-forward: %v", err)
	}
	if !reflect.DeepEqual(got["pod-1"]["dev-1"], want) {
		t.Errorf("data lost across refusal + roll-forward:\n got %#v\nwant %#v", got["pod-1"]["dev-1"], want)
	}
}

// bbolt holds an exclusive file lock: when old and new DaemonSet pods briefly
// overlap during a rolling update, the second open must fail after its
// timeout (crash, restart, retry) instead of corrupting the database.
func TestCheckpointSingleWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod_configs.db")
	cp1, err := newBoltCheckpointer(path)
	if err != nil {
		t.Fatalf("newBoltCheckpointer: %v", err)
	}

	if _, err := newBoltCheckpointer(path); err == nil {
		t.Fatal("second concurrent open should fail while the lock is held")
	}

	// the old pod exits, the retry succeeds
	cp1.Close()
	cp2, err := newBoltCheckpointer(path)
	if err != nil {
		t.Fatalf("open after lock released: %v", err)
	}
	cp2.Close()
}
