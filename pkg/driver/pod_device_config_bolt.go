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
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
	"k8s.io/apimachinery/pkg/types"
)

// Bucket layout:
//
//	pod_configs (root bucket)
//	  └── <POD_UID> (nested bucket per pod)
//	        └── device_configs (nested bucket for device configs)
//	              └── <deviceName> = <JSON-encoded DeviceConfig>
var (
	podConfigsBucket = []byte("pod_configs")
	deviceConfigsKey = []byte("device_configs")
)

// BoltCheckpointer implements Checkpointer backed by bbolt.
type BoltCheckpointer struct {
	db *bolt.DB
}

// Compile-time interface check.
var _ Checkpointer = &BoltCheckpointer{}

// NewBoltCheckpointer opens (or creates) a bbolt database at the given path.
func NewBoltCheckpointer(path string) (*BoltCheckpointer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("create pod config db directory: %w", err)
	}

	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open pod config db: %w", err)
	}

	// Ensure the root bucket exists.
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(podConfigsBucket)
		return err
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize pod config db bucket: %w", err)
	}

	return &BoltCheckpointer{db: db}, nil
}

func (c *BoltCheckpointer) Close() error {
	return c.db.Close()
}

func (c *BoltCheckpointer) Store(podUID types.UID, deviceName string, config DeviceConfig) error {
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

func (c *BoltCheckpointer) GetOrCreate() (map[types.UID]map[string]DeviceConfig, error) {
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

func (c *BoltCheckpointer) DeletePod(podUID types.UID) error {
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
