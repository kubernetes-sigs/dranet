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
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/cloudprovider/discovery"
	"sigs.k8s.io/dranet/pkg/cloudprovider/webhook"
	"sigs.k8s.io/dranet/pkg/driver"
	"sigs.k8s.io/dranet/pkg/features"
	"sigs.k8s.io/dranet/pkg/inventory"
	"sigs.k8s.io/dranet/pkg/pcidb"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	nodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/klog/v2"
)

const (
	driverName = "dra.net"
)

var (
	hostnameOverride  string
	kubeconfig        string
	bindAddress       string
	celExpression     string
	dbPath            string
	minPollInterval   time.Duration
	maxPollInterval   time.Duration
	pollBurst         int
	moveIBInterfaces  bool
	cloudProviderHint string
	profileProvider   string
	webhookURL        string
	featureGates      string

	kubeletRootDir string

	ready atomic.Bool
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&bindAddress, "bind-address", ":9177", "The IP address and port for the metrics and healthz server to serve on")
	flag.StringVar(&hostnameOverride, "hostname-override", "", "If non-empty, will be used as the name of the Node that kube-network-policies is running on. If unset, the node name is assumed to be the same as the node's hostname.")
	flag.StringVar(&celExpression, "filter", `!("dra.net/type" in attributes) || attributes["dra.net/type"].StringValue  != "veth"`, "CEL expression to filter network interface attributes (v1.DeviceAttribute).")
	flag.StringVar(&dbPath, "db-path", filepath.Join("/var/run/dranet", "dranet.db"), "Path to the persistent bbolt database file. Set to an empty string to disable persistence and use in-memory state.")
	flag.DurationVar(&minPollInterval, "inventory-min-poll-interval", 2*time.Second, "The minimum interval between two consecutive polls of the inventory.")
	flag.DurationVar(&maxPollInterval, "inventory-max-poll-interval", 1*time.Minute, "The maximum interval between two consecutive polls of the inventory.")
	flag.IntVar(&pollBurst, "inventory-poll-burst", 5, "The number of polls that can be run in a burst.")
	flag.BoolVar(&moveIBInterfaces, "move-ib-interfaces", true, "If true, InfiniBand (IPoIB) network interfaces associated with PCI devices are moved into pod network namespace. If false, moving IB network interfaces are skipped and the underlying device is exposed as an IB-only RDMA device.")
	flag.StringVar(&cloudProviderHint, "cloud-provider-hint", "", "Hint for the cloud provider that will be used to select the appropriate provider plugin. Supported values: (AWS, GCE, AZURE, OKE, ALIBABA, webhook, NONE). If left unset, the cloud provider is auto-detected.")
	flag.StringVar(&profileProvider, "profile-provider", "cloud", "Provides user intent (cloud, webhook, none). 'cloud' falls back to the cloud-provider's native implementation.")
	flag.StringVar(&webhookURL, "webhook-url", "", "URL for the webhook provider (required if using webhook for either provider)")
	flag.StringVar(&kubeletRootDir, "kubelet-root-dir", "/var/lib/kubelet", "The kubelet data directory (its --root-dir). The driver's registration socket lives under <dir>/plugins_registry and its dra.sock under <dir>/plugins/<driver-name>. Set this to match the kubelet --root-dir on clusters that relocate it.")
	flag.StringVar(&featureGates, "feature-gates", "", "A set of key=value pairs that describe feature gates for alpha/experimental features.")

	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: dranet [options]\n\n")
		flag.PrintDefaults()
	}
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	if featureGates != "" {
		if err := features.DefaultMutableFeatureGate.Set(featureGates); err != nil {
			klog.Fatalf("Failed to set feature gates: %v", err)
		}
	}

	printVersion()
	flag.VisitAll(func(f *flag.Flag) {
		klog.Infof("FLAG: --%s=%q", f.Name, f.Value)
	})

	mux := http.NewServeMux()
	// Add healthz handler
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	})
	// Add metrics handler
	mux.Handle("/metrics", promhttp.Handler())
	go func() {
		_ = http.ListenAndServe(bindAddress, mux)
	}()

	if err := pcidb.Setup(); err != nil {
		klog.Fatalf("Failed to setup PCI DB: %v", err)
	}

	var config *rest.Config
	var err error
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		// creates the in-cluster config
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		klog.Fatalf("can not create client-go configuration: %v", err)
	}

	// use protobuf for better performance at scale
	// https://kubernetes.io/docs/reference/using-api/api-concepts/#alternate-representations-of-resources
	config.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	config.ContentType = "application/vnd.kubernetes.protobuf"

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("can not create client-go client: %v", err)
	}

	nodeName, err := nodeutil.GetHostname(hostnameOverride)
	if err != nil {
		klog.Fatalf("can not obtain the node name, use the hostname-override flag if you want to set it to a specific value: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Trap signals for graceful shutdown.
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	opts := []driver.Option{}

	if dbPath != "" {
		opts = append(opts, driver.WithDBPath(dbPath))
	}

	opts = append(opts, driver.WithKubeletRootDir(kubeletRootDir))

	if celExpression != "" {
		env, err := cel.NewEnv(
			ext.NativeTypes(
				reflect.ValueOf(resourcev1.DeviceAttribute{}),
			),
			cel.Variable("attributes", cel.MapType(cel.StringType, cel.ObjectType("v1.DeviceAttribute"))),
		)
		if err != nil {
			klog.Fatalf("error creating CEL environment: %v", err)
		}
		ast, issues := env.Compile(celExpression)
		if issues != nil && issues.Err() != nil {
			klog.Fatalf("type-check error: %s", issues.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			klog.Fatalf("program construction error: %s", err)
		}
		opts = append(opts, driver.WithFilter(prg))
	}
	cloudInst, profProv, err := setupProviders(ctx, cloudProviderHint, profileProvider, webhookURL)
	if err != nil {
		klog.Fatalf("failed to setup providers: %v", err)
	}

	optsDb := []inventory.Option{
		inventory.WithRateLimiter(rate.NewLimiter(rate.Every(minPollInterval), pollBurst)),
		inventory.WithMaxPollInterval(maxPollInterval),
		inventory.WithMoveIBInterfaces(moveIBInterfaces),
	}

	if cloudInst != nil {
		optsDb = append(optsDb, inventory.WithCloudInstance(cloudInst))
	}
	if profProv != nil {
		optsDb = append(optsDb, inventory.WithProfileProvider(profProv))
	}

	db := inventory.New(optsDb...)
	opts = append(opts, driver.WithInventory(db))
	dranet, err := driver.Start(ctx, driverName, clientset, nodeName, opts...)
	if err != nil {
		klog.Fatalf("driver failed to start: %v", err)
	}
	defer dranet.Stop(cancel)

	ready.Store(true)
	klog.Info("driver started")

	select {
	case sig := <-signalCh:
		klog.Infof("Received shutdown signal: %q. Initiating graceful shutdown...", sig)
	case <-ctx.Done():
		klog.Info("Context cancelled. Initiating graceful shutdown...")
	}
}

func printVersion() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	var vcsRevision, vcsTime string
	for _, f := range info.Settings {
		switch f.Key {
		case "vcs.revision":
			vcsRevision = f.Value
		case "vcs.time":
			vcsTime = f.Value
		}
	}
	klog.Infof("dranet go %s build: %s time: %s", info.GoVersion, vcsRevision, vcsTime)
}

func setupProviders(ctx context.Context, cloudProviderHint string, profileProvider string, webhookURL string) (cloudprovider.CloudInstance, cloudprovider.ProfileProvider, error) {
	var cloudInst cloudprovider.CloudInstance
	var profProv cloudprovider.ProfileProvider
	var err error

	var hint discovery.CloudProviderHint
	// Auto-discover cloud provider if not explicitly set
	if cloudProviderHint == "" {
		hint = discovery.DiscoverCloudProvider(ctx, webhookURL)
	} else {
		hint = discovery.CloudProviderHint(cloudProviderHint)
	}

	// Setup the Underlay (Hardware Discovery / Cloud Instance Info)
	cloudInst, err = discovery.GetInstanceProperties(ctx, hint, webhookURL)
	if err != nil {
		klog.Infof("failed to initialize cloud provider %q: %v", hint, err)
		cloudInst = nil
	}

	// Setup the Overlay (Profile Provider / User Intent)
	switch profileProvider {
	case "cloud":
		if p, ok := cloudInst.(cloudprovider.ProfileProvider); ok {
			profProv = p
		} else {
			profProv = nil
		}
	case "webhook":
		if webhookURL == "" {
			return nil, nil, fmt.Errorf("--webhook-url is required when using the webhook profile provider")
		}
		var wh *webhook.WebhookProvider
		if existing, ok := cloudInst.(*webhook.WebhookProvider); ok {
			wh = existing
		} else {
			wh, err = webhook.NewWebhookProvider(ctx, webhookURL)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to initialize webhook profile provider: %v", err)
			}
		}

		if !wh.HasProfileProvider() {
			return nil, nil, fmt.Errorf("webhook at %q does not support ProfileProvider capability", webhookURL)
		}
		profProv = wh
	case "none":
		profProv = nil
	default:
		return nil, nil, fmt.Errorf("unsupported profile provider: %s", profileProvider)
	}

	return cloudInst, profProv, nil
}
