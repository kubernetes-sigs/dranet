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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
	"k8s.io/apimachinery/pkg/types"
)

// Bucket layout:
//
//	meta (root bucket)
//	  └── schemaVersion = decimal string, see checkpointSchemaVersion
//	pod_configs (root bucket)
//	  └── <POD_UID> (nested bucket per pod)
//	        └── device_configs (nested bucket for device configs)
//	              └── <deviceName> = <JSON-encoded DeviceConfig>
var (
	podConfigsBucket = []byte("pod_configs")
	deviceConfigsKey = []byte("device_configs")
	metaBucket       = []byte("meta")
	schemaVersionKey = []byte("schemaVersion")
)

// checkpointSchemaVersion is the checkpoint layout version this build reads
// and writes. A database without a meta bucket predates versioning and is
// treated as version 1. Bump this together with a checkpointMigrations entry
// whenever the DeviceConfig wire format or the bucket layout changes in a
// non-additive way.
const checkpointSchemaVersion = 1

// checkpointMigrations[v] upgrades the layout from version v to v+1 inside
// the transaction that also stamps the new version, so an interrupted
// migration rolls back as a unit.
var checkpointMigrations = map[int]func(tx *bolt.Tx) error{}

// boltCheckpointer implements Checkpointer backed by bbolt.
type boltCheckpointer struct {
	db *bolt.DB
}

// Compile-time interface check.
var _ Checkpointer = &boltCheckpointer{}

// newBoltCheckpointer opens (or creates) a bbolt database at the given path.
func newBoltCheckpointer(path string) (*boltCheckpointer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("create pod config db directory: %w", err)
	}

	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open pod config db: %w", err)
	}

	// Ensure the root bucket exists and the layout is at the current schema
	// version, migrating older layouts in the same transaction.
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(podConfigsBucket); err != nil {
			return err
		}
		return migrateSchema(tx, checkpointSchemaVersion, checkpointMigrations)
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize pod config db: %w", err)
	}

	return &boltCheckpointer{db: db}, nil
}

// migrateSchema upgrades the checkpoint layout to target one version at a
// time and stamps the version, all inside the caller's transaction so a
// failure rolls back to the previous layout untouched. A database newer than
// target is refused: checkpointed state such as DHCP leases cannot be rebuilt
// from the system, so failing loudly beats silently dropping it.
func migrateSchema(tx *bolt.Tx, target int, migrations map[int]func(tx *bolt.Tx) error) error {
	meta, err := tx.CreateBucketIfNotExists(metaBucket)
	if err != nil {
		return err
	}
	version := 1
	if raw := meta.Get(schemaVersionKey); raw != nil {
		version, err = strconv.Atoi(string(raw))
		if err != nil || version < 1 {
			return fmt.Errorf("malformed checkpoint schema version %q", raw)
		}
	}
	if version > target {
		return fmt.Errorf("checkpoint schema version %d is newer than the supported %d, refusing to load it: remove the db to discard the newer state", version, target)
	}
	for ; version < target; version++ {
		migrate, ok := migrations[version]
		if !ok {
			return fmt.Errorf("no migration from checkpoint schema version %d", version)
		}
		if err := migrate(tx); err != nil {
			return fmt.Errorf("migrate checkpoint schema from version %d to %d: %w", version, version+1, err)
		}
	}
	return meta.Put(schemaVersionKey, []byte(strconv.Itoa(target)))
}

func (c *boltCheckpointer) Close() error {
	return c.db.Close()
}

func (c *boltCheckpointer) Store(podUID types.UID, deviceName string, config DeviceConfig) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(podConfigsBucket)
		if root == nil {
			return berrors.ErrBucketNotFound
		}
		podBucket, err := root.CreateBucketIfNotExists([]byte(podUID))
		if err != nil {
			return err
		}
		devBucket, err := podBucket.CreateBucketIfNotExists(deviceConfigsKey)
		if err != nil {
			return err
		}
		data, err := json.Marshal(config)
		if err != nil {
			return err
		}
		return devBucket.Put([]byte(deviceName), data)
	})
}

func (c *boltCheckpointer) GetOrCreate() (map[types.UID]map[string]DeviceConfig, error) {
	result := make(map[types.UID]map[string]DeviceConfig)
	err := c.db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(podConfigsBucket)
		if root == nil {
			return nil
		}
		return root.ForEach(func(podUID, v []byte) error {
			if v != nil {
				return nil // not a nested bucket, skip
			}
			podBucket := root.Bucket(podUID)
			if podBucket == nil {
				return nil
			}
			devBucket := podBucket.Bucket(deviceConfigsKey)
			if devBucket == nil {
				return nil
			}
			devices := make(map[string]DeviceConfig)
			err := devBucket.ForEach(func(deviceName, data []byte) error {
				if data == nil {
					return nil // skip nested buckets
				}
				var cfg DeviceConfig
				if err := json.Unmarshal(data, &cfg); err != nil {
					return fmt.Errorf("corrupted device config for pod %s device %s: %w", string(podUID), string(deviceName), err)
				}
				devices[string(deviceName)] = cfg
				return nil
			})
			if err != nil {
				return err
			}
			if len(devices) > 0 {
				result[types.UID(string(podUID))] = devices
			}
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("read pod config checkpoint: %w", err)
	}
	return result, nil
}

func (c *boltCheckpointer) DeletePod(podUID types.UID) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(podConfigsBucket)
		if root == nil {
			return nil
		}
		err := root.DeleteBucket([]byte(podUID))
		if err == berrors.ErrBucketNotFound {
			return nil
		}
		return err
	})
}
