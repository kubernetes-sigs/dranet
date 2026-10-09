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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/klog/v2"
	"sigs.k8s.io/dranet/internal/nlwrap"
)

// unpinBPFPrograms runs in the host namespace to delete all the pinned bpf programs
func unpinBPFPrograms(ctx context.Context, ifName string) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "interface", ifName)
	device, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return err
	}
	ifIndex := uint32(device.Attrs().Index)

	logger.V(2).Info("Attempting to unpin eBPF programs from interface")
	return filepath.Walk("/sys/fs/bpf", func(pinPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		l, err := link.LoadPinnedLink(pinPath, &ebpf.LoadPinOptions{})
		if err != nil {
			logger.V(4).Info("Error getting pinned link", "path", pinPath, "err", err)
			return nil
		}

		linkInfo, err := l.Info()
		if err != nil {
			logger.Error(err, "Error getting link info", "path", pinPath)
			return nil
		}

		var linkIfIndex uint32
		switch linkInfo.Type {
		case link.TCXType:
			extra := linkInfo.TCX()
			if extra != nil {
				linkIfIndex = extra.Ifindex
			}
		case link.NetkitType:
			extra := linkInfo.Netkit()
			if extra != nil {
				linkIfIndex = extra.Ifindex
			}
		case link.XDPType:
			extra := linkInfo.XDP()
			if extra != nil {
				linkIfIndex = extra.Ifindex
			}
		default:
			return nil
		}
		if linkIfIndex != ifIndex {
			return nil
		}
		err = l.Unpin()
		if err != nil {
			logger.Error(err, "Failed to unpin bpf link", "path", pinPath, "linkID", linkInfo.ID)
		} else {
			logger.V(2).Info("Successfully unpinned bpf link", "path", pinPath, "linkID", linkInfo.ID)
		}
		return nil
	})

}

// detachEBPFPrograms detaches all eBPF programs (TC and TCX) from a given network interface.
// It attempts to remove both classic TC filters and newer TCX programs.
// It runs inside the network namespace to avoid programs on the root namespace
// to cause issues detaching the programs.
func detachEBPFPrograms(ctx context.Context, containerNsPAth string, ifName string) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "interface", ifName, "netns", containerNsPAth)
	origns, err := netns.Get()
	if err != nil {
		return fmt.Errorf("unexpected error trying to get namespace: %v", err)
	}
	defer origns.Close()
	containerNs, err := netns.GetFromPath(containerNsPAth)
	if err != nil {
		return fmt.Errorf("could not get network namespace from path %s for network device %s : %w", containerNsPAth, ifName, err)
	}
	defer containerNs.Close()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	err = netns.Set(containerNs)
	if err != nil {
		return fmt.Errorf("failed to join network namespace %s : %v", containerNsPAth, err)
	}
	// Switch back to the original namespace
	defer netns.Set(origns) // nolint:errcheck

	var errs []error
	device, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return err
	}

	// Detach TC filters (legacy)
	logger.V(2).Info("Attempting to detach TC filters from interface")
	for _, parent := range []uint32{netlink.HANDLE_MIN_INGRESS, netlink.HANDLE_MIN_EGRESS} {
		filters, err := nlwrap.FilterList(device, parent)
		if err != nil {
			logger.V(4).Info("Could not list TC filters for interface", "parent", parent, "err", err)
			continue
		}
		for _, f := range filters {
			if bpfFilter, ok := f.(*netlink.BpfFilter); ok {
				logger.V(4).Info("Deleting TC filter from interface", "filter", bpfFilter.Name, "parent", parent)
				if err := netlink.FilterDel(f); err != nil {
					logger.V(2).Info("Failed to delete TC filter", "filter", bpfFilter.Name, "parent", parent, "err", err)
				}
			}
		}
	}

	// Detach TCX programs
	logger.V(2).Info("Attempting to detach TCX programs from interface")
	for _, attach := range []ebpf.AttachType{ebpf.AttachTCXIngress, ebpf.AttachTCXEgress} {
		logger.V(2).Info("Attempting to detach programs from attachment", "attachType", attach.String())
		result, err := link.QueryPrograms(link.QueryOptions{
			Target: int(device.Attrs().Index),
			Attach: attach,
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, p := range result.Programs {
			logger.V(2).Info("Attempting to detach program from interface", "programID", p.ID, "attachType", attach.String())
			err = tryDetach(klog.NewContext(ctx, logger), p.ID, device.Attrs().Index, attach)
			if err != nil {
				logger.V(2).Info("Failed to detach program from interface", "programID", p.ID, "attachType", attach.String(), "err", err)
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

func tryDetach(ctx context.Context, id ebpf.ProgramID, deviceIdx int, attach ebpf.AttachType) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "programID", id)
	prog, err := ebpf.NewProgramFromID(id)
	if err != nil {
		logger.V(2).Info("Failed to get eBPF program", "err", err)
		return err
	}

	if err := prog.Unpin(); err != nil {
		logger.Error(err, "Failed to unpin eBPF program", "program", prog.String())
		return err
	}

	err = link.RawDetachProgram(link.RawDetachProgramOptions{
		Target:  deviceIdx,
		Program: prog,
		Attach:  attach,
	})
	if err != nil {
		logger.V(2).Info("Failed to detach eBPF program", "err", err)
	}

	err = prog.Close()
	if err != nil {
		logger.Error(err, "Failed to close eBPF program", "program", prog.String())
		return err
	}
	return nil
}
