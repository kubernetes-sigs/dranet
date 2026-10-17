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

package ocihook

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// The container runtime executes the hook on the host, so the hook's path
// must resolve there. DRANET usually runs in a container, where its own
// executable path means nothing to the host, and installing a copy on the
// host is fragile: /run is commonly noexec, /opt read-only. /proc/<pid>/exe
// of the daemon, with the daemon's PID in the runtime's PID namespace,
// executes the daemon's binary wherever it lives, since the exec permission
// is checked on the file, not on /proc.
//
// The daemon learns that PID in one of two ways:
//   - in the initial PID namespace (a process on the host, or a container
//     with hostPID), os.Getpid() is it;
//   - otherwise, the runtime reports the daemon's own container, with its
//     host PID, when the NRI plugin synchronizes. Identity holds what the
//     daemon knows about itself to find that container.

// procPIDInitIno is the inode number of the initial PID namespace, fixed by
// the kernel (PROC_PID_INIT_INO in include/linux/proc_ns.h).
const procPIDInitIno = 0xEFFFFFFC

// Identity is what the daemon can learn about its own process and container
// from inside.
type Identity struct {
	// InitPIDNamespace is true when the process runs in the initial PID
	// namespace, so PID is valid for the container runtime too.
	InitPIDNamespace bool
	// PID is the process ID in its own PID namespace.
	PID int
	// Args are the process arguments, as the runtime knows them.
	Args []string
	// PodUID is the Kubernetes Pod UID found in the kubelet directories
	// mounted into the container, empty when the process is not in a Pod.
	PodUID string
	// ContainerName is the container name found with the PodUID, if any.
	ContainerName string
}

// SelfIdentity inspects the current process.
func SelfIdentity() (Identity, error) {
	id := Identity{PID: os.Getpid(), Args: append([]string(nil), os.Args...)}
	initNS, err := inInitPIDNamespace("/proc/self/ns/pid")
	if err != nil {
		return id, err
	}
	id.InitPIDNamespace = initNS
	mountInfo, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return id, fmt.Errorf("read the mounts: %w", err)
	}
	defer mountInfo.Close()
	id.PodUID, id.ContainerName, err = podFromMountInfo(mountInfo)
	if err != nil {
		return id, err
	}
	return id, nil
}

// HostExecutable returns the path, valid in the initial PID namespace, of
// the executable of the process with that PID.
func HostExecutable(pid int) string {
	return "/proc/" + strconv.Itoa(pid) + "/exe"
}

func inInitPIDNamespace(nsPath string) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(nsPath, &st); err != nil {
		return false, fmt.Errorf("stat %s: %w", nsPath, err)
	}
	return st.Ino == procPIDInitIno, nil
}

// podFromMountInfo finds the Pod a container belongs to in its mounts: the
// kubelet mounts files from <root-dir>/pods/<pod UID>/ into every container
// (the termination log, projected volumes), and the termination log is under
// containers/<container name>/. It returns empty values when no mount comes
// from a Pod directory, and an error when mounts disagree on the Pod.
func podFromMountInfo(r io.Reader) (podUID, containerName string, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		// mount ID, parent ID, major:minor, root, mount point, options, ...
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		uid, name := podDirectory(unescapeMountField(fields[3]))
		if uid == "" {
			continue
		}
		if podUID != "" && uid != podUID {
			return "", "", fmt.Errorf("mounts come from two Pods, %s and %s", podUID, uid)
		}
		podUID = uid
		if name != "" {
			if containerName != "" && name != containerName {
				return "", "", fmt.Errorf("mounts come from two containers, %s and %s", containerName, name)
			}
			containerName = name
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("read the mounts: %w", err)
	}
	return podUID, containerName, nil
}

// podDirectory extracts the Pod UID, and the container name when the path
// is under the containers directory, from a path in a kubelet Pod directory.
func podDirectory(path string) (podUID, containerName string) {
	const marker = "/pods/"
	index := strings.Index(path, marker)
	if index < 0 {
		return "", ""
	}
	uid, rest, found := strings.Cut(path[index+len(marker):], "/")
	if !found || uid == "" {
		return "", ""
	}
	if rest, found := strings.CutPrefix(rest, "containers/"); found {
		if name, _, found := strings.Cut(rest, "/"); found && name != "" {
			return uid, name
		}
	}
	return uid, ""
}

// unescapeMountField decodes the octal escapes of mountinfo paths.
func unescapeMountField(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var b strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if value, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(value))
				i += 3
				continue
			}
		}
		b.WriteByte(field[i])
	}
	return b.String()
}
