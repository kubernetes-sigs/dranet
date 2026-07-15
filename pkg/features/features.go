package features

import (
	"k8s.io/component-base/featuregate"
)

const (
	// PersistentResourceSliceAttributes gates the persistence of network
	// attributes (like MAC, MTU, etc.) in the ResourceSlice across daemon restarts.
	// owner: @purvavj
	// alpha: v0.1.0
	PersistentResourceSliceAttributes featuregate.Feature = "PersistentResourceSliceAttributes"
)

// DefaultMutableFeatureGate is a mutable feature gate used only for registration
// and testing.
var DefaultMutableFeatureGate featuregate.MutableFeatureGate = featuregate.NewFeatureGate()

// DefaultFeatureGate is a read-only view of the feature gate. You should use
// this throughout your code to check if a feature is enabled.
var DefaultFeatureGate featuregate.FeatureGate = DefaultMutableFeatureGate

func init() {
	err := DefaultMutableFeatureGate.Add(map[featuregate.Feature]featuregate.FeatureSpec{
		PersistentResourceSliceAttributes: {
			Default:    false,
			PreRelease: featuregate.Alpha,
		},
	})
	if err != nil {
		panic(err)
	}
}
