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
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/types"
	resourceapply "k8s.io/client-go/applyconfigurations/resource/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/internal/nlwrap"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

// BenchmarkAttachNetdevPhases breaks down the kernel and netlink operations
// performed for each NIC during RunPodSandbox:
//
//  1. LinkByName + LinkSetDown on the host: acquires rtnl_lock and calls the
//     driver's ndo_stop (on mlx5_core, mlx5e_close tears down all RX/TX
//     channels, CQs, SQs, RQs, and TIRs via synchronous PCIe firmware commands).
//  2. RTM_NEWLINK with IFLA_NET_NS_FD (dev_change_net_namespace): acquires
//     rtnl_lock, runs NETDEV_UNREGISTER notifiers (including mlx5_ib and
//     roce_gid_mgmt workqueue flush), calls rcu_barrier(), moves the net_device,
//     runs NETDEV_REGISTER notifiers, and calls synchronize_net(). Even on a
//     software dummy interface with no hardware, rcu_barrier() + synchronize_net()
//     under rtnl_lock costs ~25-55ms per NIC move.
//  3. Per-interface sysctl, AddrAdd, LinkSetUp in the Pod netns: LinkSetUp
//     acquires rtnl_lock and calls ndo_open (on mlx5_core, mlx5e_open allocates
//     and creates all RX/TX channels via synchronous PCIe firmware commands).
//  4. Ethtool features/priv-flags and routing/rules configuration.
func BenchmarkAttachNetdevPhases(b *testing.B) {
	userns.RunBenchmark(b, benchmarkAttachNetdevPhases, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func benchmarkAttachNetdevPhases(b *testing.B) {
	origns, err := netns.Get()
	if err != nil {
		b.Fatalf("get namespace: %v", err)
	}
	defer origns.Close()

	nsName := fmt.Sprintf("benchns%x", time.Now().UnixNano())
	containerNsPath := filepath.Join("/run/netns", nsName)
	podNS, err := netns.NewNamed(nsName)
	if err != nil {
		b.Fatalf("create network namespace: %v", err)
	}
	defer netns.DeleteNamed(nsName)
	defer podNS.Close()
	if err := netns.Set(origns); err != nil {
		b.Fatal(err)
	}

	const hostIfName = "bench0"
	const podIfName = "dranet0"
	la := netlink.NewLinkAttrs()
	la.Name = hostIfName
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		b.Fatalf("add dummy: %v", err)
	}
	defer func() {
		if l, err := nlwrap.LinkByName(hostIfName); err == nil {
			_ = netlink.LinkDel(l)
		}
	}()
	hostDev, err := nlwrap.LinkByName(hostIfName)
	if err != nil {
		b.Fatal(err)
	}
	if err := netlink.LinkSetUp(hostDev); err != nil {
		b.Fatal(err)
	}

	b.Run("1_LinkByName_Host", func(b *testing.B) {
		for b.Loop() {
			if _, err := nlwrap.LinkByName(hostIfName); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("2_LinkSetDown_And_Up_Host", func(b *testing.B) {
		for b.Loop() {
			if err := netlink.LinkSetDown(hostDev); err != nil {
				b.Fatal(err)
			}
			if err := netlink.LinkSetUp(hostDev); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("3_NetnsMove_IFLA_NET_NS_FD_RoundTrip", func(b *testing.B) {
		var moveToPod, moveToHost time.Duration
		var n int
		for b.Loop() {
			n++
			t0 := time.Now()
			if err := moveLinkBetweenNS(netns.None(), podNS, hostDev.Attrs().Index, podIfName); err != nil {
				b.Fatal(err)
			}
			moveToPod += time.Since(t0)

			t1 := time.Now()
			if err := moveLinkBetweenNS(podNS, origns, hostDev.Attrs().Index, hostIfName); err != nil {
				b.Fatal(err)
			}
			moveToHost += time.Since(t1)
		}
		if n > 0 {
			b.ReportMetric(float64(moveToPod.Milliseconds())/float64(n), "ms/move-to-pod")
			b.ReportMetric(float64(moveToHost.Milliseconds())/float64(n), "ms/move-to-host")
		}
	})

	b.Run("4_NewHandleAt_And_LinkByName_PodNS", func(b *testing.B) {
		for b.Loop() {
			nhNs, err := nlwrap.NewHandleAt(podNS)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := nhNs.LinkByName("lo"); err != nil {
				nhNs.Close()
				b.Fatal(err)
			}
			nhNs.Close()
		}
	})

	b.Run("5_Sysctl_PodNS", func(b *testing.B) {
		if err := moveLinkBetweenNS(netns.None(), podNS, hostDev.Attrs().Index, podIfName); err != nil {
			b.Fatal(err)
		}
		defer func() {
			_ = moveLinkBetweenNS(podNS, origns, hostDev.Attrs().Index, hostIfName)
		}()
		ifCfg := apis.InterfaceConfig{
			Name:        podIfName,
			ARPIgnore:   ptr.To[int32](1),
			ARPAnnounce: ptr.To[int32](2),
			AcceptRA:    ptr.To[int32](0),
		}
		b.ResetTimer()
		for b.Loop() {
			if err := applyInterfaceSysctlConfig(podNS, podIfName, ifCfg); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("6_Ethtool_FeaturesSet_PodNS", func(b *testing.B) {
		if err := moveLinkBetweenNS(netns.None(), podNS, hostDev.Attrs().Index, podIfName); err != nil {
			b.Fatal(err)
		}
		defer func() {
			_ = moveLinkBetweenNS(podNS, origns, hostDev.Attrs().Index, hostIfName)
		}()
		ethCfg := &apis.EthtoolConfig{
			Features: map[string]bool{"tx-checksum-ip-generic": true},
		}
		b.ResetTimer()
		for b.Loop() {
			if err := applyEthtoolConfig(containerNsPath, podIfName, ethCfg); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("7_FullAttachConfigureAndDetach_1NIC", func(b *testing.B) {
		cfg := DeviceConfig{
			Claim: types.NamespacedName{Namespace: "ns", Name: "claim1"},
			NetworkInterfaceConfigInHost: apis.NetworkConfig{
				Interface: apis.InterfaceConfig{Name: hostIfName},
			},
			NetworkInterfaceConfigInPod: apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Name:        podIfName,
					Addresses:   []string{"10.10.0.1/24"},
					MTU:         ptr.To[int32](1400),
					ARPIgnore:   ptr.To[int32](1),
					ARPAnnounce: ptr.To[int32](2),
				},
				Routes: []apis.RouteConfig{
					{Destination: "10.20.0.0/24", Gateway: "10.10.0.254"},
				},
				Ethtool: &apis.EthtoolConfig{
					Features: map[string]bool{"tx-checksum-ip-generic": true},
				},
			},
		}
		var attachTotal, configTotal, detachTotal time.Duration
		var n int
		b.ResetTimer()
		for b.Loop() {
			n++
			t0 := time.Now()
			data, err := nsAttachNetdev(hostIfName, containerNsPath, cfg.NetworkInterfaceConfigInPod.Interface)
			if err != nil {
				b.Fatal(err)
			}
			attachTotal += time.Since(t0)

			t1 := time.Now()
			if err := configureNetdevInNS(context.Background(), containerNsPath, "dev0", cfg, data.InterfaceName, resourceapply.AllocatedDeviceStatus()); err != nil {
				b.Fatal(err)
			}
			configTotal += time.Since(t1)

			t2 := time.Now()
			if err := nsDetachNetdev(containerNsPath, podIfName, hostIfName); err != nil {
				b.Fatal(err)
			}
			detachTotal += time.Since(t2)
		}
		if n > 0 {
			b.ReportMetric(float64(attachTotal.Milliseconds())/float64(n), "ms/attach")
			b.ReportMetric(float64(configTotal.Milliseconds())/float64(n), "ms/config")
			b.ReportMetric(float64(detachTotal.Milliseconds())/float64(n), "ms/detach")
		}
	})
}

// BenchmarkAttachManyDevices compares attaching 1, 2, 4, and 8 NICs
// sequentially vs. concurrently across goroutines. Because the Linux kernel
// serializes every dev_change_net_namespace (IFLA_NET_NS_FD) on the global
// rtnl_lock across rcu_barrier() and synchronize_net(), moving N NICs scales
// linearly (~N * per-NIC kernel move time) even when issued from N goroutines.
func BenchmarkAttachManyDevices(b *testing.B) {
	userns.RunBenchmark(b, benchmarkAttachManyDevices, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func benchmarkAttachManyDevices(b *testing.B) {
	origns, err := netns.Get()
	if err != nil {
		b.Fatalf("get namespace: %v", err)
	}
	defer origns.Close()

	nsName := fmt.Sprintf("benchmany%x", time.Now().UnixNano())
	containerNsPath := filepath.Join("/run/netns", nsName)
	podNS, err := netns.NewNamed(nsName)
	if err != nil {
		b.Fatalf("create network namespace: %v", err)
	}
	defer netns.DeleteNamed(nsName)
	defer podNS.Close()
	if err := netns.Set(origns); err != nil {
		b.Fatal(err)
	}

	deviceConfig := func(i int) DeviceConfig {
		return DeviceConfig{
			Claim: types.NamespacedName{Namespace: "ns", Name: "claim1"},
			NetworkInterfaceConfigInHost: apis.NetworkConfig{
				Interface: apis.InterfaceConfig{Name: fmt.Sprintf("bmany%d", i)},
			},
			NetworkInterfaceConfigInPod: apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Name:      fmt.Sprintf("dranet%d", i),
					Addresses: []string{fmt.Sprintf("10.10.%d.1/24", i)},
					MTU:       ptr.To[int32](1400),
				},
				Routes: []apis.RouteConfig{
					{Destination: fmt.Sprintf("10.20.%d.0/24", i), Gateway: fmt.Sprintf("10.10.%d.254", i)},
				},
			},
		}
	}

	for _, numDevices := range []int{1, 2, 4, 8} {
		for i := range numDevices {
			la := netlink.NewLinkAttrs()
			la.Name = fmt.Sprintf("bmany%d", i)
			if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
				b.Fatalf("add dummy %s: %v", la.Name, err)
			}
		}

		b.Run(fmt.Sprintf("%dNICs_Sequential", numDevices), func(b *testing.B) {
			var attachDur time.Duration
			var n int
			for b.Loop() {
				n++
				t0 := time.Now()
				for i := range numDevices {
					cfg := deviceConfig(i)
					data, err := nsAttachNetdev(cfg.NetworkInterfaceConfigInHost.Interface.Name, containerNsPath, cfg.NetworkInterfaceConfigInPod.Interface)
					if err != nil {
						b.Fatal(err)
					}
					if err := configureNetdevInNS(context.Background(), containerNsPath, fmt.Sprintf("dev%d", i), cfg, data.InterfaceName, resourceapply.AllocatedDeviceStatus()); err != nil {
						b.Fatal(err)
					}
				}
				attachDur += time.Since(t0)
				b.StopTimer()
				for i := range numDevices {
					if err := nsDetachNetdev(containerNsPath, fmt.Sprintf("dranet%d", i), fmt.Sprintf("bmany%d", i)); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
			}
			if n > 0 {
				msTotal := float64(attachDur.Milliseconds()) / float64(n)
				b.ReportMetric(msTotal, "ms/attach-all")
				b.ReportMetric(msTotal/float64(numDevices), "ms/nic")
			}
		})

		b.Run(fmt.Sprintf("%dNICs_Parallel", numDevices), func(b *testing.B) {
			var attachDur time.Duration
			var n int
			for b.Loop() {
				n++
				t0 := time.Now()
				var wg sync.WaitGroup
				errCh := make(chan error, numDevices)
				for i := range numDevices {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						cfg := deviceConfig(i)
						data, err := nsAttachNetdev(cfg.NetworkInterfaceConfigInHost.Interface.Name, containerNsPath, cfg.NetworkInterfaceConfigInPod.Interface)
						if err != nil {
							errCh <- err
							return
						}
						if err := configureNetdevInNS(context.Background(), containerNsPath, fmt.Sprintf("dev%d", i), cfg, data.InterfaceName, resourceapply.AllocatedDeviceStatus()); err != nil {
							errCh <- err
						}
					}(i)
				}
				wg.Wait()
				close(errCh)
				if err := <-errCh; err != nil {
					b.Fatal(err)
				}
				attachDur += time.Since(t0)
				b.StopTimer()
				for i := range numDevices {
					if err := nsDetachNetdev(containerNsPath, fmt.Sprintf("dranet%d", i), fmt.Sprintf("bmany%d", i)); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
			}
			if n > 0 {
				msTotal := float64(attachDur.Milliseconds()) / float64(n)
				b.ReportMetric(msTotal, "ms/attach-all")
				b.ReportMetric(msTotal/float64(numDevices), "ms/nic")
			}
		})

		b.Run(fmt.Sprintf("%dNICs_PreDownAndSharedHandle", numDevices), func(b *testing.B) {
			var attachDur time.Duration
			var n int
			for b.Loop() {
				b.StopTimer()
				for i := range numDevices {
					l, err := nlwrap.LinkByName(fmt.Sprintf("bmany%d", i))
					if err != nil {
						b.Fatal(err)
					}
					if err := netlink.LinkSetDown(l); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()

				n++
				t0 := time.Now()
				nsHandle := newPodNetnsHandle(containerNsPath)
				for i := range numDevices {
					cfg := deviceConfig(i)
					data, nsLink, err := nsAttachNetdevWithHandle(nsHandle, cfg.NetworkInterfaceConfigInHost.Interface.Name, cfg.NetworkInterfaceConfigInPod.Interface)
					if err != nil {
						nsHandle.Close()
						b.Fatal(err)
					}
					if err := configureNetdevInNSWithHandle(context.Background(), containerNsPath, nsHandle, nsLink, fmt.Sprintf("dev%d", i), cfg, data.InterfaceName, resourceapply.AllocatedDeviceStatus()); err != nil {
						nsHandle.Close()
						b.Fatal(err)
					}
				}
				nsHandle.Close()
				attachDur += time.Since(t0)

				b.StopTimer()
				for i := range numDevices {
					if err := nsDetachNetdev(containerNsPath, fmt.Sprintf("dranet%d", i), fmt.Sprintf("bmany%d", i)); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
			}
			if n > 0 {
				msTotal := float64(attachDur.Milliseconds()) / float64(n)
				b.ReportMetric(msTotal, "ms/attach-all")
				b.ReportMetric(msTotal/float64(numDevices), "ms/nic")
			}
		})

		for i := range numDevices {
			if l, err := nlwrap.LinkByName(fmt.Sprintf("bmany%d", i)); err == nil {
				_ = netlink.LinkDel(l)
			}
		}
	}
}

func moveLinkBetweenNS(fromNS, toNS netns.NsHandle, linkIndex int, newName string) error {
	s, err := nl.GetNetlinkSocketAt(fromNS, netns.None(), unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer s.Close()

	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	req.Sockets = map[int]*nl.SocketHandle{
		unix.NETLINK_ROUTE: {Socket: s},
	}
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(linkIndex)
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(newName)))
	req.AddData(nl.NewRtAttr(unix.IFLA_NET_NS_FD, nl.Uint32Attr(uint32(toNS))))
	_, err = req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil && err != net.ErrClosed {
		return err
	}
	return nil
}
