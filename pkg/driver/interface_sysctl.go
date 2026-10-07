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
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/vishvananda/netns"
	"k8s.io/component-helpers/node/util/sysctl"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/pkg/apis"
)

// sysctlProvider is overridden in tests to exercise sysctl failure paths.
var sysctlProvider = sysctl.New

// hasInterfaceSysctlConfig reports whether the interface config asks for any per-interface sysctl.
func hasInterfaceSysctlConfig(interfaceConfig apis.InterfaceConfig) bool {
	return interfaceConfig.ARPIgnore != nil || interfaceConfig.ARPAnnounce != nil ||
		interfaceConfig.AcceptRA != nil || interfaceConfig.DADTransmits != nil ||
		interfaceConfig.RouterSolicitationDelay != nil || interfaceConfig.RouterSolicitationInterval != nil ||
		interfaceConfig.DisableIPv6 != nil ||
		interfaceConfig.AddrGenMode != nil
}

func applyInterfaceSysctlsWithSysctl(sysctlInterface sysctl.Interface, ifName string, interfaceConfig apis.InterfaceConfig) error {
	var errorList []error
	set := func(family, setting string, value int32) {
		name := fmt.Sprintf("net/%s/conf/%s/%s", family, ifName, setting)
		if err := sysctlInterface.SetSysctl(name, int(value)); err != nil {
			errorList = append(errorList, fmt.Errorf("failed to set %s: %w", name, err))
		}
	}

	if interfaceConfig.ARPIgnore != nil {
		set("ipv4", "arp_ignore", *interfaceConfig.ARPIgnore)
	}
	if interfaceConfig.ARPAnnounce != nil {
		set("ipv4", "arp_announce", *interfaceConfig.ARPAnnounce)
	}
	// The address generation mode decides the interface identifier of the
	// link-local address, which the kernel generates as soon as IPv6 comes up,
	// so it has to be in place before disable_ipv6 is cleared below.
	if interfaceConfig.AddrGenMode != nil {
		set("ipv6", "addr_gen_mode", *interfaceConfig.AddrGenMode)
	}
	// Set the solicitation and duplicate address detection behaviour before
	// accepting advertisements, so the first solicitation the interface sends
	// already uses the requested timing.
	if interfaceConfig.DADTransmits != nil {
		set("ipv6", "dad_transmits", *interfaceConfig.DADTransmits)
	}
	if interfaceConfig.RouterSolicitationDelay != nil {
		set("ipv6", "router_solicitation_delay", *interfaceConfig.RouterSolicitationDelay)
	}
	if interfaceConfig.RouterSolicitationInterval != nil {
		set("ipv6", "router_solicitation_interval", *interfaceConfig.RouterSolicitationInterval)
	}
	if interfaceConfig.AcceptRA != nil {
		name := fmt.Sprintf("net/ipv6/conf/%s/accept_ra", ifName)
		err := sysctlInterface.SetSysctl(name, int(*interfaceConfig.AcceptRA))
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist) && *interfaceConfig.AcceptRA == 0:
			// The interface has no IPv6 sysctls, so it accepts no router
			// advertisements and zero is already satisfied.
			klog.V(4).Infof("%s not found; IPv6 is not enabled on %s and acceptRA: 0 is already satisfied", name, ifName)
		case errors.Is(err, os.ErrNotExist):
			errorList = append(errorList, fmt.Errorf("failed to set %s: IPv6 is not enabled on the interface: %w", name, err))
		default:
			errorList = append(errorList, fmt.Errorf("failed to set %s: %w", name, err))
		}
	}
	// Last, because enabling IPv6 starts address configuration with whatever
	// the settings above are at that moment.
	if interfaceConfig.DisableIPv6 != nil {
		name := fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", ifName)
		value := 0
		if *interfaceConfig.DisableIPv6 {
			value = 1
		}
		err := sysctlInterface.SetSysctl(name, value)
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist) && *interfaceConfig.DisableIPv6:
			// The interface has no IPv6 sysctls, so IPv6 is already off.
			klog.V(4).Infof("%s not found; IPv6 is not enabled on %s and disableIPv6: true is already satisfied", name, ifName)
		case errors.Is(err, os.ErrNotExist):
			errorList = append(errorList, fmt.Errorf("failed to set %s: IPv6 is not available on the interface: %w", name, err))
		default:
			errorList = append(errorList, fmt.Errorf("failed to set %s: %w", name, err))
		}
	}
	return errors.Join(errorList...)
}

// applyInterfaceSysctlConfig sets the requested per-interface sysctls inside the
// Pod network namespace. These live under /proc/sys, so unlike the rest of the
// interface configuration they cannot be set through a netlink handle and
// require joining the namespace.
func applyInterfaceSysctlConfig(containerNs netns.NsHandle, ifName string, interfaceConfig apis.InterfaceConfig) error {
	if !hasInterfaceSysctlConfig(interfaceConfig) {
		return nil
	}

	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		unlockThread := true
		defer func() {
			if unlockThread {
				runtime.UnlockOSThread()
			}
		}()

		originalNs, err := netns.Get()
		if err != nil {
			result <- fmt.Errorf("failed to get current network namespace: %w", err)
			return
		}
		defer originalNs.Close()

		if err := netns.Set(containerNs); err != nil {
			result <- fmt.Errorf("failed to join target network namespace: %w", err)
			return
		}

		applyErr := applyInterfaceSysctlsWithSysctl(sysctlProvider(), ifName, interfaceConfig)
		if err := netns.Set(originalNs); err != nil {
			// Keep this thread locked so the runtime destroys it instead of
			// reusing it in the wrong network namespace.
			unlockThread = false
			result <- errors.Join(applyErr, fmt.Errorf("failed to restore network namespace: %w", err))
			return
		}
		result <- applyErr
	}()
	return <-result
}
