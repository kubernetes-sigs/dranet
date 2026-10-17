package features

import (
	"k8s.io/component-base/featuregate"
)

const (
	// DRAListTypeAttributes enables list-valued standardized NUMA attributes.
	DRAListTypeAttributes featuregate.Feature = "DRAListTypeAttributes"

	// PersistentResourceSliceAttributes gates the persistence of device
	// attributes (like MAC, MTU, etc.) in the ResourceSlice across daemon restarts.
	// owner: @purvavj
	// alpha: v1.4.0
	PersistentResourceSliceAttributes featuregate.Feature = "PersistentResourceSliceAttributes"

	// RuntimeProvider gates executing configured RuntimeProviders via an OCI
	// createRuntime hook before a Pod's first container starts.
	// owner: @aojea
	// alpha: v1.5.0
	RuntimeProvider featuregate.Feature = "RuntimeProvider"
)

// DefaultMutableFeatureGate is a mutable feature gate used only for registration
// and testing.
var DefaultMutableFeatureGate featuregate.MutableFeatureGate = featuregate.NewFeatureGate()

// DefaultFeatureGate is a read-only view of the feature gate. You should use
// this throughout your code to check if a feature is enabled.
var DefaultFeatureGate featuregate.FeatureGate = DefaultMutableFeatureGate

func init() {
	err := DefaultMutableFeatureGate.Add(map[featuregate.Feature]featuregate.FeatureSpec{
		// The `DRAListTypeAttributes` feature gate in DRANET must also be enabled in the Kubernetes control plane.
		// If this is enabled in DRANET but not the control plane ResourceSlices will not be published.
		DRAListTypeAttributes: {
			Default:    false,
			PreRelease: featuregate.Alpha,
		},
		PersistentResourceSliceAttributes: {
			Default:    false,
			PreRelease: featuregate.Alpha,
		},
		RuntimeProvider: {
			Default:    false,
			PreRelease: featuregate.Alpha,
		},
	})
	if err != nil {
		panic(err)
	}
}
