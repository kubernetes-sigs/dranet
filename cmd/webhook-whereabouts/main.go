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

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider/webhook"
)

type cniNetConf struct {
	cnitypes.NetConf
	rawBytes []byte
}

type Server struct {
	binDir   string
	profiles map[string]cniNetConf
}

func (s *Server) GetDeviceAttributes(w http.ResponseWriter, r *http.Request) {
	// Not used for whereabouts IPAM
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("{}"))
}

func (s *Server) GetDeviceConfig(w http.ResponseWriter, r *http.Request) {
	// Not used for whereabouts IPAM
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("{}"))
}

// pciNamePrefix is the prefix dranet prepends to a normalized PCI address
// (pkg/names.NormalizePCIAddress: "0000:8a:00.0" -> "pci-0000-8a-00-0").
const pciNamePrefix = "pci-"

// cniIfname returns the CNI_IFNAME to pass to the whereabouts IPAM plugin.
//
// whereabouts matches a reservation for release on (CNI_CONTAINERID, CNI_IFNAME)
// but never creates an interface from it, so the value just needs to be (1) a
// valid CNI interface name and (2) identical at ADD and DEL, else the DEL fails
// to match and the IP leaks. We derive it from the stable Device.Name rather
// than the kernel ifName (which is mutable across netns moves) so (2) holds by
// construction, and keep it human-readable so leaked reservations can be traced
// back during manual pool cleanup:
//
//   - already valid (short, non-PCI names): used verbatim;
//   - PCI names exceed IFNAMSIZ only due to the "pci-" prefix, so we strip it
//     ("pci-0000-27-00-2" -> "0000-27-00-2"), re-validating the result;
//   - anything still invalid (e.g. base32-encoded names, already unreadable):
//     a deterministic hash.
func cniIfname(req webhook.ProfileRequest) string {
	name := req.Device.Name
	if isValidCNIIfname(name) {
		return name
	}
	// Re-validate after stripping: never emit an over-length name on the
	// assumption that dropping the prefix made it fit.
	if trimmed := strings.TrimPrefix(name, pciNamePrefix); trimmed != name && isValidCNIIfname(trimmed) {
		return trimmed
	}
	// "dra" + 8 hex digits (FNV-1a/32) = 11 bytes: always valid, deterministic
	// for a given device identifier, and distinct per device within a claim.
	h := fnv.New32a()
	h.Write([]byte(name))
	return fmt.Sprintf("dra%08x", h.Sum32())
}

// isValidCNIIfname reports whether s would be accepted by the CNI runtime's
// interface-name validation. It mirrors github.com/containernetworking/cni
// pkg/utils.ValidateInterfaceName.
func isValidCNIIfname(s string) bool {
	if len(s) == 0 || len(s) > apis.MaxInterfaceNameLen {
		return false
	}
	if s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if r == '/' || r == ':' || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func (s *Server) GetProfileConfig(w http.ResponseWriter, r *http.Request) {
	var req webhook.ProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var profileName string
	if req.Config != nil {
		profileName = req.Config.Profile
	}

	conf, exists := s.profiles[profileName]
	if !exists {
		http.Error(w, fmt.Sprintf("CNI profile %q not found", profileName), http.StatusNotFound)
		return
	}

	binPath := filepath.Join(s.binDir, filepath.Base(conf.IPAM.Type))
	env := []string{
		"CNI_COMMAND=ADD",
		"CNI_CONTAINERID=" + string(req.ClaimUID),
		"CNI_NETNS=/dev/null", // IPAM plugins don't need network namespaces
		"CNI_IFNAME=" + cniIfname(req),
		"CNI_PATH=" + s.binDir,
		"CNI_ARGS=IgnoreUnknown=1;K8S_POD_NAMESPACE=default;K8S_POD_NAME=pod-whereabouts;K8S_POD_INFRA_CONTAINER_ID=" + string(req.ClaimUID),
	}

	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(conf.rawBytes)

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			http.Error(w, fmt.Sprintf("IPAM plugin %s failed: %s", conf.IPAM.Type, string(exitErr.Stderr)), http.StatusInternalServerError)
			return
		}
		http.Error(w, fmt.Sprintf("failed to execute IPAM plugin: %v", err), http.StatusInternalServerError)
		return
	}

	var res types100.Result
	if err := json.Unmarshal(output, &res); err != nil {
		http.Error(w, fmt.Sprintf("failed to parse CNI IPAM result: %v", err), http.StatusInternalServerError)
		return
	}

	config := apis.NetworkConfig{}
	for _, ip := range res.IPs {
		config.Interface.Addresses = append(config.Interface.Addresses, ip.Address.String())
	}
	for _, r := range res.Routes {
		gw := ""
		if r.GW != nil {
			gw = r.GW.String()
		}
		config.Routes = append(config.Routes, apis.RouteConfig{
			Destination: r.Dst.String(),
			Gateway:     gw,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(config)
}

func (s *Server) ReleaseProfileConfig(w http.ResponseWriter, r *http.Request) {
	var req webhook.ProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var profileName string
	if req.Config != nil {
		profileName = req.Config.Profile
	}

	conf, exists := s.profiles[profileName]
	if !exists {
		w.WriteHeader(http.StatusOK)
		return
	}

	binPath := filepath.Join(s.binDir, filepath.Base(conf.IPAM.Type))
	env := []string{
		"CNI_COMMAND=DEL",
		"CNI_CONTAINERID=" + string(req.ClaimUID),
		"CNI_NETNS=/dev/null",
		"CNI_IFNAME=" + cniIfname(req),
		"CNI_PATH=" + s.binDir,
		"CNI_ARGS=IgnoreUnknown=1;K8S_POD_NAMESPACE=default;K8S_POD_NAME=pod-whereabouts;K8S_POD_INFRA_CONTAINER_ID=" + string(req.ClaimUID),
	}

	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(conf.rawBytes)

	output, err := cmd.CombinedOutput()
	if err != nil {
		http.Error(w, fmt.Sprintf("IPAM plugin DEL failed: %v, output: %s", err, string(output)), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) Health(w http.ResponseWriter, r *http.Request) {
	caps := webhook.Capabilities{
		ProfileProvider: true,
		CloudProvider:   false,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(caps)
}

func main() {
	var bindAddress string
	var binDir string
	var confDir string

	flag.StringVar(&bindAddress, "bind-address", ":8080", "The IP address and port for the webhook server to serve on")
	flag.StringVar(&binDir, "cni-bin-dir", "/opt/cni/bin", "CNI binaries directory")
	flag.StringVar(&confDir, "cni-conf-dir", "/etc/cni/net.d", "CNI config directory")
	flag.Parse()

	if stat, err := os.Stat(binDir); err != nil || !stat.IsDir() {
		log.Fatalf("CNI bin dir %q is invalid or missing", binDir)
	}
	if stat, err := os.Stat(confDir); err != nil || !stat.IsDir() {
		log.Fatalf("CNI conf dir %q is invalid or missing", confDir)
	}

	server := &Server{
		binDir:   binDir,
		profiles: make(map[string]cniNetConf),
	}

	files, err := os.ReadDir(confDir)
	if err != nil {
		log.Fatalf("failed to read CNI conf dir: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || (filepath.Ext(f.Name()) != ".conf" && filepath.Ext(f.Name()) != ".json") {
			continue
		}

		rawBytes, err := os.ReadFile(filepath.Join(confDir, f.Name()))
		if err != nil {
			log.Fatalf("failed to read config %s: %v", f.Name(), err)
		}

		var conf cniNetConf
		if err := json.Unmarshal(rawBytes, &conf); err != nil {
			log.Printf("Skipping invalid JSON in CNI config %s", f.Name())
			continue
		}

		if conf.Name == "" || conf.IPAM.Type == "" {
			continue
		}
		if conf.Type != "" {
			log.Fatalf("CNI profile %q should not specify a 'type', only 'name' and 'ipam' are allowed to avoid confusion", f.Name())
		}

		binPath := filepath.Join(binDir, filepath.Base(conf.IPAM.Type))
		info, err := os.Stat(binPath)
		if err != nil || info.Mode()&0111 == 0 {
			log.Fatalf("IPAM binary %q required by profile %q is missing or not executable", binPath, conf.Name)
		}

		conf.rawBytes = rawBytes
		server.profiles[conf.Name] = conf
		log.Printf("Loaded CNI Profile %q mapped to IPAM %q", conf.Name, conf.IPAM.Type)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(webhook.PathHealth, server.Health)
	mux.HandleFunc(webhook.PathGetDeviceAttributes, server.GetDeviceAttributes)
	mux.HandleFunc(webhook.PathGetDeviceConfig, server.GetDeviceConfig)
	mux.HandleFunc(webhook.PathGetProfileConfig, server.GetProfileConfig)
	mux.HandleFunc(webhook.PathReleaseProfileConfig, server.ReleaseProfileConfig)

	log.Printf("Starting webhook provider on %s", bindAddress)
	if err := http.ListenAndServe(bindAddress, mux); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
