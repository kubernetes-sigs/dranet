package driver

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/stub"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/dranet/pkg/apis"
)

// fakeDraPlugin is a mock implementation of the pluginHelper interface for testing.
type fakePluginHelper struct {
	publishErr         error
	publishCalled      chan struct{}
	registrationStatus *registerapi.RegistrationStatus
	stopCalled         atomic.Bool
}

func newFakePluginHelper() *fakePluginHelper {
	return &fakePluginHelper{
		publishCalled: make(chan struct{}, 1),
	}
}

func (m *fakePluginHelper) PublishResources(_ context.Context, _ resourceslice.DriverResources) error {
	if m.publishCalled != nil {
		m.publishCalled <- struct{}{}
	}
	return m.publishErr
}

func (m *fakePluginHelper) Stop() {
	m.stopCalled.Store(true)
}

func (m *fakePluginHelper) RegistrationStatus() *registerapi.RegistrationStatus {
	return m.registrationStatus
}

// mockNetDB is a mock implementation of the inventoryDB interface for testing.
type fakeInventoryDB struct {
	resources           chan []resourcev1.Device
	rescanCalls         atomic.Int32
	GetDeviceFunc       func(deviceName string) (resourcev1.Device, bool)
	GetDeviceConfigFunc func(deviceName string) (*apis.NetworkConfig, bool)
	GetNetInterfaceNameFunc func(deviceName string) (string, error)
	IsIBOnlyDeviceFunc      func(deviceName string) bool
	GetProfileConfigFunc    func(deviceName string, claimUID types.UID, config *apis.NetworkConfig) (*apis.NetworkConfig, error)
	ReleaseProfileConfigFunc func(deviceName string, claimUID types.UID, config *apis.NetworkConfig) error
}

func newFakeInventoryDB() *fakeInventoryDB {
	return &fakeInventoryDB{
		resources: make(chan []resourcev1.Device, 1),
	}
}

func (m *fakeInventoryDB) Run(_ context.Context) error { return nil }

func (m *fakeInventoryDB) GetResources(_ context.Context) <-chan []resourcev1.Device {
	return m.resources
}

func (m *fakeInventoryDB) GetDevice(deviceName string) (resourcev1.Device, bool) {
	if m.GetDeviceFunc != nil {
		return m.GetDeviceFunc(deviceName)
	}
	return resourcev1.Device{}, false
}

func (m *fakeInventoryDB) GetNetInterfaceName(deviceName string) (string, error) {
	if m.GetNetInterfaceNameFunc != nil {
		return m.GetNetInterfaceNameFunc(deviceName)
	}
	return "", nil
}

func (m *fakeInventoryDB) IsIBOnlyDevice(deviceName string) bool {
	if m.IsIBOnlyDeviceFunc != nil {
		return m.IsIBOnlyDeviceFunc(deviceName)
	}
	return false
}

func (m *fakeInventoryDB) GetRDMADeviceName(_ string) (string, error) { return "", nil }

func (m *fakeInventoryDB) GetDeviceConfig(deviceName string) (*apis.NetworkConfig, bool) {
	if m.GetDeviceConfigFunc != nil {
		return m.GetDeviceConfigFunc(deviceName)
	}
	return nil, false
}

func (m *fakeInventoryDB) RequestRescan() {
	m.rescanCalls.Add(1)
}

func (m *fakeInventoryDB) GetProfileConfig(deviceName string, claimUID types.UID, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	if m.GetProfileConfigFunc != nil {
		return m.GetProfileConfigFunc(deviceName, claimUID, config)
	}
	return nil, nil
}

func (m *fakeInventoryDB) ReleaseProfileConfig(deviceName string, claimUID types.UID, config *apis.NetworkConfig) error {
	if m.ReleaseProfileConfigFunc != nil {
		return m.ReleaseProfileConfigFunc(deviceName, claimUID, config)
	}
	return nil
}

// fakeNriStub is a mock implementation of the stub.Stub interface for testing.
type fakeNriStub struct {
	stub.Stub
	stopCalled bool
}

func (m *fakeNriStub) Stop() {
	m.stopCalled = true
}

func (m *fakeNriStub) Run(_ context.Context) error {
	return nil
}

func TestStop(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	fakeDra := newFakePluginHelper()
	fakeNri := &fakeNriStub{}

	np := &NetworkDriver{
		draPlugin:      fakeDra,
		nriPlugin:      fakeNri,
		podConfigStore: mustNewPodConfigStore(),
		clock:          fakeClock,
	}

	podUID1 := types.UID("pod-1")
	podUID2 := types.UID("pod-2")

	// Pod 1: Prepared but no NRI activity
	np.podConfigStore.SetDeviceConfig(podUID1, "random-dev-1", DeviceConfig{})

	// Pod 2: Prepared and has recent NRI activity
	np.podConfigStore.SetDeviceConfig(podUID2, "random-dev-1", DeviceConfig{})
	np.podConfigStore.UpdateLastNRIActivity(podUID2, fakeClock.Now())

	cancelCalled := false
	cancel := func() {
		cancelCalled = true
	}

	// Run Stop in a separate goroutine because it will block
	stopDone := make(chan struct{})
	go func() {
		np.Stop(cancel)
		close(stopDone)
	}()

	// 1. Initially, Stop should be waiting.
	select {
	case <-stopDone:
		t.Fatal("Stop() returned prematurely while pods were still in flight")
	case <-time.After(100 * time.Millisecond):
		// Success: still waiting
	}

	if !fakeDra.stopCalled.Load() {
		t.Errorf("draPlugin.Stop() should have been called immediately")
	}

	// 2. Advance clock slightly.
	// Pod 1 still has no activity, and the overall wait time is less than maxWaitPeriod,
	// so Stop should still wait.
	fakeClock.Step(5 * time.Second)

	select {
	case <-stopDone:
		t.Fatal("Stop() returned while Pod 1 was still waiting for activity")
	case <-time.After(100 * time.Millisecond):
		// Success: still waiting
	}

	// 3. Advance clock again to satisfy grace period for Pod 2. At the same time,
	// although Pod 1 still has no NRI activity, it has been waiting too long
	// since prepareResourceClaim so both Pods now satisfy their wait conditions.
	fakeClock.Step(5 * time.Second)

	// 4. Now Stop should finish.
	select {
	case <-stopDone:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("Stop() timed out after all pods were stable")
	}

	if !cancelCalled {
		t.Errorf("cancel() was not called")
	}
	if !fakeNri.stopCalled {
		t.Errorf("nriPlugin.Stop() was not called")
	}
}
