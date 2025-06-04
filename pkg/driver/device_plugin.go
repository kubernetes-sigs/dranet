/*
Copyright 2025 Google LLC

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
	"path"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

const (
	// https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/
	kubeletSocket = "kubelet.sock"
	pluginSocket  = "dranet.sock"
	pluginName    = "dranet"
	resourceName  = "dra.net"
)

var _ registerapi.RegistrationServer = &devicePlugin{}
var _ pluginapi.DevicePluginServer = &devicePlugin{}

type devicePlugin struct {
	Version      string
	ResourceName string
	Endpoint     string
	Name         string
	Type         string

	s *grpc.Server

	mu            sync.Mutex
	registered    bool
	registerError error
}

func newPlugin() *devicePlugin {
	// https://github.com/cncf-tags/container-device-interface/blob/main/SPEC.md
	return &devicePlugin{
		Version:      pluginapi.Version,
		ResourceName: resourceName,
		Type:         registerapi.DevicePlugin,
		Endpoint:     path.Join(pluginapi.DevicePluginPath, pluginSocket),
	}
}

func (p *devicePlugin) run(ctx context.Context) error {
	socket, err := net.Listen("unix", p.Endpoint)
	if err != nil {
		return err
	}

	p.s = grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(p.s, p)

	go func() {
		err = p.s.Serve(socket)
		if err != nil {
			klog.Infof("Server stopped listening: %v", err)
		}
	}()

	// wait until grpc server is ready
	for i := 0; i < 10; i++ {
		services := p.s.GetServiceInfo()
		if len(services) >= 1 {
			break
		}
		time.Sleep(1 * time.Second)
	}
	klog.Infof("Server is ready listening on: %s", socket.Addr().String())
	// register the plugin
	err = p.register(ctx)
	if err != nil {
		p.s.Stop()
		socket.Close()
		return err
	}

	// Cleanup if socket is cancelled
	go func() {
		<-ctx.Done()
		p.s.Stop()
		socket.Close()
	}()
	return nil
}

// register the plugin in the kubelet
func (p *devicePlugin) register(ctx context.Context) error {
	ctx, timeoutCancel := context.WithTimeout(ctx, 1*time.Minute)
	defer timeoutCancel()

	conn, err := grpc.DialContext(ctx, "unix://"+path.Join(pluginapi.DevicePluginPath, kubeletSocket), grpc.WithBlock(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect %s, %v", path.Join(pluginapi.DevicePluginPath, kubeletSocket), err)
	}
	defer conn.Close()
	klog.Info("connected to the kubelet")

	client := pluginapi.NewRegistrationClient(conn)
	_, err = client.Register(ctx, &pluginapi.RegisterRequest{
		Version:      p.Version,
		Endpoint:     pluginSocket,
		ResourceName: p.ResourceName,
		Options: &pluginapi.DevicePluginOptions{
			PreStartRequired: false,
		},
	})
	if err != nil {
		klog.Errorf("%s: Registration failed: %v", p.Name, err)
		return err
	}
	klog.Info("connected to the kubelet")

	return nil
}

func (p *devicePlugin) GetInfo(context.Context, *registerapi.InfoRequest) (*registerapi.PluginInfo, error) {
	klog.V(2).Infof("GetInfo request")
	return &registerapi.PluginInfo{
		Type: p.Type,
		Name: p.Name,
	}, nil
}

func (p *devicePlugin) NotifyRegistrationStatus(ctx context.Context, status *registerapi.RegistrationStatus) (*registerapi.RegistrationStatusResponse, error) {
	klog.V(2).Infof("NotifyRegistrationStatus request: %v", status)
	p.mu.Lock()
	defer p.mu.Unlock()
	if status.PluginRegistered {
		klog.Infof("%s gets registered successfully at Kubelet \n", pluginName)
		p.registered = true
		p.registerError = nil
	} else {
		klog.Infof("%s failed to be registered at Kubelet: %v; restarting.\n", pluginName, status.Error)
		p.registered = false
		p.registerError = fmt.Errorf(status.Error)
	}
	return &registerapi.RegistrationStatusResponse{}, nil
}

func (p *devicePlugin) GetPreferredAllocation(ctx context.Context, in *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	klog.V(2).Infof("GetPreferredAllocation request: %v", in)
	return &pluginapi.PreferredAllocationResponse{}, nil
}

func (p *devicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	klog.V(2).Infof("ListAndWatch request")
	for {
		response := pluginapi.ListAndWatchResponse{}
		response.Devices = append(response.Devices, &pluginapi.Device{})
		// update kubelet
		err := s.Send(&response)
		if err != nil {
			klog.V(2).Infof("Error sending message %v", err)
			continue
		}

	}
}

// GetDevicePluginOptions returns options to be communicated with Device Manager
func (p *devicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (
	*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		PreStartRequired: false,
	}, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase
func (p *devicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (
	*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// Allocate which return list of devices.
func (p *devicePlugin) Allocate(ctx context.Context, in *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	klog.V(2).Infof("Allocate request: %v", in)
	p.mu.Lock()
	defer p.mu.Unlock()
	out := &v1beta1.AllocateResponse{
		ContainerResponses: make([]*v1beta1.ContainerAllocateResponse, 0, len(in.ContainerRequests)),
	}
	for _, request := range in.GetContainerRequests() {
		// Pass the CDI device plugin with annotations or environment variables
		// and add a hook on the CDI plugin that reads this and perform the
		// ip link ethX set netns NS
		resp := v1beta1.ContainerAllocateResponse{}
		for _, id := range request.DevicesIDs {
			klog.V(2).Infof("Allocate request interface: %s", id)

		}
		out.ContainerResponses = append(out.ContainerResponses, &resp)
	}
	klog.V(2).Infof("Allocate request response: %v", out)
	return out, nil
}
