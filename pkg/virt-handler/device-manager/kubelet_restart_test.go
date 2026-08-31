/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright the KubeVirt Authors.
 *
 */

package device_manager

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"

	pluginapi "kubevirt.io/kubevirt/pkg/virt-handler/device-manager/deviceplugin/v1beta1"
)

type fakeKubeletRegistrationServer struct {
	pluginDir string

	lock                 sync.Mutex
	registrations        []*pluginapi.RegisterRequest
	deviceLists          map[int][][]*pluginapi.Device
	pluginConnections    []*grpc.ClientConn
	receivers            sync.WaitGroup
	beforeRegisterReturn func()
}

func newFakeKubeletRegistrationServer(pluginDir string) *fakeKubeletRegistrationServer {
	return &fakeKubeletRegistrationServer{
		pluginDir:   pluginDir,
		deviceLists: make(map[int][][]*pluginapi.Device),
	}
}

func (f *fakeKubeletRegistrationServer) Register(_ context.Context, request *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	conn, err := gRPCConnect(filepath.Join(f.pluginDir, request.Endpoint), connectionTimeout)
	if err != nil {
		return nil, err
	}

	stream, err := pluginapi.NewDevicePluginClient(conn).ListAndWatch(context.Background(), &pluginapi.Empty{})
	if err != nil {
		conn.Close()
		return nil, err
	}

	requestCopy := *request
	f.lock.Lock()
	f.registrations = append(f.registrations, &requestCopy)
	registration := len(f.registrations)
	f.pluginConnections = append(f.pluginConnections, conn)
	f.receivers.Add(1)
	f.lock.Unlock()

	go func() {
		defer f.receivers.Done()
		for {
			response, err := stream.Recv()
			if err != nil {
				return
			}
			devices := make([]*pluginapi.Device, len(response.Devices))
			copy(devices, response.Devices)
			f.lock.Lock()
			f.deviceLists[registration] = append(f.deviceLists[registration], devices)
			f.lock.Unlock()
		}
	}()

	if f.beforeRegisterReturn != nil {
		f.beforeRegisterReturn()
	}
	return &pluginapi.Empty{}, nil
}

func (f *fakeKubeletRegistrationServer) registrationCount() int {
	f.lock.Lock()
	defer f.lock.Unlock()
	return len(f.registrations)
}

func (f *fakeKubeletRegistrationServer) lastListLength(registration int) int {
	f.lock.Lock()
	defer f.lock.Unlock()
	lists := f.deviceLists[registration]
	if len(lists) == 0 {
		return -1
	}
	return len(lists[len(lists)-1])
}

func (f *fakeKubeletRegistrationServer) closePluginStreams() {
	f.lock.Lock()
	connections := append([]*grpc.ClientConn(nil), f.pluginConnections...)
	f.pluginConnections = nil
	f.lock.Unlock()

	for _, conn := range connections {
		_ = conn.Close()
	}
	f.receivers.Wait()
}

type fakeKubelet struct {
	socketPath   string
	registration *fakeKubeletRegistrationServer
	server       *grpc.Server
	serveDone    chan struct{}
}

func newFakeKubelet(socketPath, pluginDir string) *fakeKubelet {
	return &fakeKubelet{
		socketPath:   socketPath,
		registration: newFakeKubeletRegistrationServer(pluginDir),
	}
}

func (f *fakeKubelet) start() error {
	if err := os.Remove(f.socketPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", f.socketPath)
	if err != nil {
		return err
	}

	f.server = grpc.NewServer()
	f.serveDone = make(chan struct{})
	pluginapi.RegisterRegistrationServer(f.server, f.registration)
	go func() {
		_ = f.server.Serve(listener)
		close(f.serveDone)
	}()

	return waitForGRPCServer(f.socketPath, connectionTimeout)
}

func (f *fakeKubelet) stop() {
	f.registration.closePluginStreams()
	if f.server != nil {
		f.server.Stop()
		<-f.serveDone
		f.server = nil
	}
	_ = os.Remove(f.socketPath)
}

type blockingFakeKubelet struct {
	socketPath string
	server     *grpc.Server
	serveDone  chan struct{}
	entered    chan *pluginapi.RegisterRequest
	release    chan struct{}
	releaseOne sync.Once
}

func newBlockingFakeKubelet(socketPath string) *blockingFakeKubelet {
	return &blockingFakeKubelet{
		socketPath: socketPath,
		entered:    make(chan *pluginapi.RegisterRequest, 2),
		release:    make(chan struct{}),
	}
}

func (f *blockingFakeKubelet) Register(ctx context.Context, request *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	requestCopy := *request
	select {
	case f.entered <- &requestCopy:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.release:
		return &pluginapi.Empty{}, nil
	}
}

func (f *blockingFakeKubelet) start() error {
	if err := os.Remove(f.socketPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", f.socketPath)
	if err != nil {
		return err
	}

	f.server = grpc.NewServer()
	f.serveDone = make(chan struct{})
	pluginapi.RegisterRegistrationServer(f.server, f)
	go func() {
		_ = f.server.Serve(listener)
		close(f.serveDone)
	}()

	return waitForGRPCServer(f.socketPath, connectionTimeout)
}

func (f *blockingFakeKubelet) releaseRegistrations() {
	f.releaseOne.Do(func() { close(f.release) })
}

func (f *blockingFakeKubelet) stop() {
	f.releaseRegistrations()
	if f.server != nil {
		f.server.Stop()
		<-f.serveDone
		f.server = nil
	}
	_ = os.Remove(f.socketPath)
}

type blockingDevicePluginServer struct {
	pluginapi.UnimplementedDevicePluginServer
	entered chan struct{}
	release <-chan struct{}
}

func (s *blockingDevicePluginServer) ListAndWatch(_ *pluginapi.Empty, _ pluginapi.DevicePlugin_ListAndWatchServer) error {
	close(s.entered)
	<-s.release
	return nil
}

type restartTestPlugin struct {
	Device
	grpcPlugin    pluginapi.DevicePluginServer
	socketPath    string
	healthCheck   func(*fsnotify.Watcher) error
	setServer     func(*grpc.Server)
	stopPlugin    func() error
	resetChannels func()
	wakeHandlers  func()
}

func newTestHealthWatcher(socketPath string) *fsnotify.Watcher {
	watcher, err := fsnotify.NewWatcher()
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
	ExpectWithOffset(1, watcher.Add(filepath.Dir(socketPath))).To(Succeed())
	return watcher
}

func verifyHealthCheckReturnsForEvent(plugin *restartTestPlugin, event fsnotify.Event) {
	watcher := newTestHealthWatcher(plugin.socketPath)
	result := make(chan error, 1)
	go func() { result <- plugin.healthCheck(watcher) }()

	delivered := make(chan struct{})
	go func() {
		watcher.Events <- event
		close(delivered)
	}()

	Eventually(result, 5*time.Second).Should(Receive(BeNil()))
	Eventually(delivered, 5*time.Second).Should(BeClosed())
}

func verifyHealthCheckIgnoresEvent(plugin *restartTestPlugin, ignored, restart fsnotify.Event) {
	watcher := newTestHealthWatcher(plugin.socketPath)
	result := make(chan error, 1)
	go func() { result <- plugin.healthCheck(watcher) }()

	delivered := make(chan struct{})
	go func() {
		watcher.Events <- ignored
		close(delivered)
	}()
	Eventually(delivered, 5*time.Second).Should(BeClosed())
	Consistently(result, 200*time.Millisecond).ShouldNot(Receive())

	go func() { watcher.Events <- restart }()
	Eventually(result, 5*time.Second).Should(Receive(BeNil()))
}

func verifyActiveListAndWatchHandlerIsWoken(plugin *restartTestPlugin) {
	listener, err := net.Listen("unix", plugin.socketPath)
	Expect(err).ToNot(HaveOccurred())

	server := newDevicePluginGRPCServer()
	plugin.setServer(server)
	pluginapi.RegisterDevicePluginServer(server, plugin.grpcPlugin)
	go func() { _ = server.Serve(listener) }()
	Expect(waitForGRPCServer(plugin.socketPath, connectionTimeout)).To(Succeed())
	DeferCleanup(func() {
		plugin.wakeHandlers()
		server.Stop()
	})

	conn, err := gRPCConnect(plugin.socketPath, connectionTimeout)
	Expect(err).ToNot(HaveOccurred())
	DeferCleanup(conn.Close)
	streamContext, cancelStream := context.WithTimeout(context.Background(), 10*time.Second)
	DeferCleanup(cancelStream)
	stream, err := pluginapi.NewDevicePluginClient(conn).ListAndWatch(streamContext, &pluginapi.Empty{})
	Expect(err).ToNot(HaveOccurred())
	response, err := stream.Recv()
	Expect(err).ToNot(HaveOccurred())
	Expect(response.Devices).ToNot(BeEmpty())

	stopResult := make(chan error, 1)
	go func() { stopResult <- plugin.stopPlugin() }()
	Eventually(stopResult, 5*time.Second).Should(Receive(Succeed()))
}

var _ = Describe("Device plugin re-registration after kubelet restart", func() {
	var workDir string
	var socketDir string
	var kubeletSocketPath string
	var stop chan struct{}

	touch := func(path string) {
		file, err := os.Create(path)
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		ExpectWithOffset(1, file.Close()).To(Succeed())
	}

	BeforeEach(func() {
		var err error
		workDir, err = os.MkdirTemp("", "kubevirt-kubelet-restart")
		Expect(err).ToNot(HaveOccurred())

		Expect(os.MkdirAll(filepath.Join(workDir, "dev", "vfio"), 0755)).To(Succeed())
		touch(filepath.Join(workDir, "dev", "vfio", "42"))

		socketDir = filepath.Join(workDir, "device-plugins")
		Expect(os.MkdirAll(socketDir, 0755)).To(Succeed())
		kubeletSocketPath = filepath.Join(socketDir, filepath.Base(pluginapi.KubeletSocket))
		touch(kubeletSocketPath)
		stop = make(chan struct{})
	})

	AfterEach(func() {
		close(stop)
		Expect(os.RemoveAll(workDir)).To(Succeed())
	})

	newTestPCIPlugin := func() *restartTestPlugin {
		dpi := NewPCIDevicePlugin([]*PCIDevice{
			{pciID: "dead:beef", pciAddress: "0000:00:00.0", iommuGroup: "42", numaNode: -1},
		}, "vendor.example.org/fake-nvme")
		dpi.socketPath = filepath.Join(socketDir, "kubevirt-fake-nvme.sock")
		dpi.deviceRoot = workDir
		dpi.devicePath = filepath.Join("dev", "vfio")
		dpi.stop = stop
		return &restartTestPlugin{
			Device:      dpi,
			grpcPlugin:  dpi,
			socketPath:  dpi.socketPath,
			healthCheck: dpi.healthCheck,
			setServer:   func(server *grpc.Server) { dpi.server = server },
			stopPlugin:  dpi.stopDevicePlugin,
			resetChannels: func() {
				dpi.done = make(chan struct{})
				dpi.deregistered = make(chan struct{})
			},
			wakeHandlers: func() {
				if !IsChanClosed(dpi.done) {
					close(dpi.done)
				}
			},
		}
	}

	newTestGenericPlugin := func() *restartTestPlugin {
		devicePath := filepath.Join(workDir, "dev", "kvm")
		touch(devicePath)
		dpi := NewGenericDevicePlugin("fake-kvm", devicePath, 1, "rw", false)
		dpi.socketPath = filepath.Join(socketDir, "kubevirt-fake-kvm.sock")
		dpi.deviceRoot = "/"
		dpi.stop = stop
		return &restartTestPlugin{
			Device:      dpi,
			grpcPlugin:  dpi,
			socketPath:  dpi.socketPath,
			healthCheck: dpi.healthCheck,
			setServer:   func(server *grpc.Server) { dpi.server = server },
			stopPlugin:  dpi.stopDevicePlugin,
			resetChannels: func() {
				dpi.done = make(chan struct{})
				dpi.deregistered = make(chan struct{})
			},
			wakeHandlers: func() {
				if !IsChanClosed(dpi.done) {
					close(dpi.done)
				}
			},
		}
	}

	newTestDevicePlugin := func(kind string) *restartTestPlugin {
		if kind == "PCI" {
			return newTestPCIPlugin()
		}
		return newTestGenericPlugin()
	}

	It("waits for gRPC handlers when stopping the production server", func() {
		socketPath := filepath.Join(socketDir, "blocking-handler.sock")
		listener, err := net.Listen("unix", socketPath)
		Expect(err).ToNot(HaveOccurred())

		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
		server := newDevicePluginGRPCServer()
		serveDone := make(chan struct{})
		plugin := &blockingDevicePluginServer{
			entered: make(chan struct{}),
			release: release,
		}
		pluginapi.RegisterDevicePluginServer(server, plugin)
		go func() {
			_ = server.Serve(listener)
			close(serveDone)
		}()
		DeferCleanup(func() {
			releaseHandler()
			server.Stop()
			Eventually(serveDone, 5*time.Second).Should(BeClosed())
		})
		Expect(waitForGRPCServer(socketPath, connectionTimeout)).To(Succeed())

		conn, err := gRPCConnect(socketPath, connectionTimeout)
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(conn.Close)
		streamContext, cancelStream := context.WithCancel(context.Background())
		DeferCleanup(cancelStream)
		_, err = pluginapi.NewDevicePluginClient(conn).ListAndWatch(streamContext, &pluginapi.Empty{})
		Expect(err).ToNot(HaveOccurred())
		Eventually(plugin.entered, 5*time.Second).Should(BeClosed())

		stopped := make(chan struct{})
		go func() {
			server.Stop()
			close(stopped)
		}()
		Consistently(stopped, 300*time.Millisecond).ShouldNot(BeClosed())

		releaseHandler()
		Eventually(stopped, 5*time.Second).Should(BeClosed())
	})

	DescribeTable("watches for restarts before registering",
		func(kind string) {
			plugin := newTestDevicePlugin(kind)
			oldKubelet := newFakeKubelet(kubeletSocketPath, socketDir)
			registerEntered := make(chan struct{})
			releaseRegister := make(chan struct{})
			var enterOnce sync.Once
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseRegister) }) }
			oldKubelet.registration.beforeRegisterReturn = func() {
				enterOnce.Do(func() { close(registerEntered) })
				<-releaseRegister
			}
			Expect(oldKubelet.start()).To(Succeed())

			newKubelet := newFakeKubelet(kubeletSocketPath, socketDir)
			controlled := &controlledDevice{
				devicePlugin: plugin.Device,
				backoff:      []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
			}
			controlled.Start()
			controlledStopped := false
			DeferCleanup(func() {
				release()
				if !controlledStopped {
					controlled.Stop()
					Eventually(plugin.GetInitialized, 5*time.Second).Should(BeFalse())
				}
				newKubelet.stop()
				oldKubelet.stop()
			})

			Eventually(registerEntered, 5*time.Second).Should(BeClosed())
			Expect(newKubelet.start()).To(Succeed())
			release()

			Eventually(newKubelet.registration.registrationCount, 10*time.Second).Should(Equal(1))
			Eventually(plugin.GetInitialized, 5*time.Second).Should(BeTrue())

			controlled.Stop()
			controlledStopped = true
			Eventually(plugin.GetInitialized, 5*time.Second).Should(BeFalse())
		},
		Entry("for PCI plugins", "PCI"),
		Entry("for generic plugins", "generic"),
	)

	DescribeTable("re-registers and advertises devices on the second cycle",
		func(kind string) {
			plugin := newTestDevicePlugin(kind)
			kubelet := newFakeKubelet(kubeletSocketPath, socketDir)
			Expect(kubelet.start()).To(Succeed())
			DeferCleanup(kubelet.stop)

			controlled := &controlledDevice{
				devicePlugin: plugin.Device,
				backoff:      []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
			}
			controlled.Start()
			controlledStopped := false
			DeferCleanup(func() {
				if !controlledStopped {
					controlled.Stop()
					Eventually(plugin.GetInitialized, 5*time.Second).Should(BeFalse())
				}
			})

			Eventually(kubelet.registration.registrationCount, 5*time.Second).Should(Equal(1))
			Eventually(func() int {
				return kubelet.registration.lastListLength(1)
			}, 5*time.Second).Should(Equal(1))

			kubelet.stop()
			Expect(os.Remove(plugin.socketPath)).To(Succeed())
			Expect(kubelet.start()).To(Succeed())

			Eventually(kubelet.registration.registrationCount, 10*time.Second).Should(Equal(2))
			Eventually(func() int {
				return kubelet.registration.lastListLength(2)
			}, 5*time.Second).Should(Equal(1))
			Consistently(func() int {
				return kubelet.registration.lastListLength(2)
			}, 300*time.Millisecond).Should(Equal(1))

			controlled.Stop()
			controlledStopped = true
			Eventually(plugin.GetInitialized, 5*time.Second).Should(BeFalse())
		},
		Entry("PCI", "PCI"),
		Entry("generic", "generic"),
	)

	DescribeTable("wakes active ListAndWatch handlers before stopping the server",
		func(kind string) {
			plugin := newTestDevicePlugin(kind)
			Expect(os.RemoveAll(plugin.socketPath)).To(Succeed())
			plugin.resetChannels()
			verifyActiveListAndWatchHandlerIsWoken(plugin)
		},
		Entry("PCI", "PCI"),
		Entry("generic", "generic"),
	)

	DescribeTable("detects kubelet restart filesystem events",
		func(kind, restart string) {
			plugin := newTestDevicePlugin(kind)
			watcher := newTestHealthWatcher(plugin.socketPath)
			result := make(chan error, 1)
			go func() { result <- plugin.healthCheck(watcher) }()

			switch restart {
			case "kubelet socket":
				Expect(os.Remove(kubeletSocketPath)).To(Succeed())
				time.Sleep(100 * time.Millisecond)
				touch(kubeletSocketPath)
			case "plugin directory":
				Expect(os.Rename(socketDir, socketDir+".gone")).To(Succeed())
				Expect(os.MkdirAll(socketDir, 0755)).To(Succeed())
				touch(kubeletSocketPath)
			}

			Eventually(result, 5*time.Second).Should(Receive(BeNil()))
		},
		Entry("PCI kubelet socket recreation", "PCI", "kubelet socket"),
		Entry("generic kubelet socket recreation", "generic", "kubelet socket"),
		Entry("PCI plugin directory replacement", "PCI", "plugin directory"),
		Entry("generic plugin directory replacement", "generic", "plugin directory"),
	)

	Context("registration RPC supervision", func() {
		It("cancels hung PCI and generic registrations on shutdown", func() {
			kubelet := newBlockingFakeKubelet(kubeletSocketPath)
			Expect(kubelet.start()).To(Succeed())
			DeferCleanup(kubelet.stop)

			pci := newTestPCIPlugin()
			generic := newTestGenericPlugin()
			pciStop := make(chan struct{})
			genericStop := make(chan struct{})
			var stopPCIOnce sync.Once
			var stopGenericOnce sync.Once
			stopPCI := func() { stopPCIOnce.Do(func() { close(pciStop) }) }
			stopGeneric := func() { stopGenericOnce.Do(func() { close(genericStop) }) }
			DeferCleanup(stopPCI)
			DeferCleanup(stopGeneric)

			pciResult := make(chan error, 1)
			genericResult := make(chan error, 1)
			go func() { pciResult <- pci.Start(pciStop) }()
			go func() { genericResult <- generic.Start(genericStop) }()

			for range 2 {
				Eventually(kubelet.entered, 5*time.Second).Should(Receive())
			}

			started := time.Now()
			stopPCI()
			stopGeneric()

			var pciErr error
			var genericErr error
			Eventually(pciResult, 3*time.Second).Should(Receive(&pciErr))
			Eventually(genericResult, 3*time.Second).Should(Receive(&genericErr))
			Expect(pciErr).To(MatchError(ContainSubstring("Canceled")))
			Expect(genericErr).To(MatchError(ContainSubstring("Canceled")))
			Expect(time.Since(started)).To(BeNumerically("<", connectionTimeout))
		})

		It("times out hung PCI and generic registrations", func() {
			kubelet := newBlockingFakeKubelet(kubeletSocketPath)
			Expect(kubelet.start()).To(Succeed())
			DeferCleanup(kubelet.stop)

			pci := newTestPCIPlugin()
			generic := newTestGenericPlugin()
			pciStop := make(chan struct{})
			genericStop := make(chan struct{})
			DeferCleanup(func() { close(pciStop) })
			DeferCleanup(func() { close(genericStop) })

			pciResult := make(chan error, 1)
			genericResult := make(chan error, 1)
			started := time.Now()
			go func() { pciResult <- pci.Start(pciStop) }()
			go func() { genericResult <- generic.Start(genericStop) }()

			for range 2 {
				Eventually(kubelet.entered, 5*time.Second).Should(Receive())
			}

			var pciErr error
			var genericErr error
			Eventually(pciResult, connectionTimeout+2*time.Second).Should(Receive(&pciErr))
			Eventually(genericResult, connectionTimeout+2*time.Second).Should(Receive(&genericErr))
			Expect(pciErr).To(MatchError(ContainSubstring("DeadlineExceeded")))
			Expect(genericErr).To(MatchError(ContainSubstring("DeadlineExceeded")))
			Expect(time.Since(started)).To(BeNumerically(">=", connectionTimeout))
		})
	})

	DescribeTable("handles combined fsnotify operation masks",
		func(kind, target string, operation fsnotify.Op) {
			plugin := newTestDevicePlugin(kind)
			eventName := plugin.socketPath
			if target == "kubelet socket" {
				eventName = kubeletSocketPath
			} else if target == "plugin directory" {
				eventName = socketDir
			}
			verifyHealthCheckReturnsForEvent(plugin, fsnotify.Event{Name: eventName, Op: operation})
		},
		Entry("PCI own socket Remove|Chmod", "PCI", "plugin socket", fsnotify.Remove|fsnotify.Chmod),
		Entry("generic own socket Remove|Chmod", "generic", "plugin socket", fsnotify.Remove|fsnotify.Chmod),
		Entry("PCI kubelet socket Create|Chmod", "PCI", "kubelet socket", fsnotify.Create|fsnotify.Chmod),
		Entry("generic kubelet socket Create|Chmod", "generic", "kubelet socket", fsnotify.Create|fsnotify.Chmod),
		Entry("PCI directory Rename|Chmod", "PCI", "plugin directory", fsnotify.Rename|fsnotify.Chmod),
		Entry("generic directory Rename|Chmod", "generic", "plugin directory", fsnotify.Rename|fsnotify.Chmod),
		Entry("PCI directory Remove|Chmod", "PCI", "plugin directory", fsnotify.Remove|fsnotify.Chmod),
		Entry("generic directory Remove|Chmod", "generic", "plugin directory", fsnotify.Remove|fsnotify.Chmod),
	)

	DescribeTable("ignores restart events with the wrong path or operation",
		func(kind, target, wrongField string) {
			plugin := newTestDevicePlugin(kind)
			restart := fsnotify.Event{Name: plugin.socketPath, Op: fsnotify.Remove}
			if target == "kubelet socket" {
				restart = fsnotify.Event{Name: kubeletSocketPath, Op: fsnotify.Create}
			} else if target == "plugin directory" {
				restart = fsnotify.Event{Name: socketDir, Op: fsnotify.Rename}
			}

			ignored := restart
			if wrongField == "path" {
				ignored.Name += ".unrelated"
			} else {
				ignored.Op = fsnotify.Chmod
			}
			verifyHealthCheckIgnoresEvent(plugin, ignored, restart)
		},
		Entry("PCI ignores a kubelet create on the wrong path", "PCI", "kubelet socket", "path"),
		Entry("generic ignores a kubelet create on the wrong path", "generic", "kubelet socket", "path"),
		Entry("PCI ignores a plugin remove on the wrong path", "PCI", "plugin socket", "path"),
		Entry("generic ignores a plugin remove on the wrong path", "generic", "plugin socket", "path"),
		Entry("PCI ignores a directory rename on the wrong path", "PCI", "plugin directory", "path"),
		Entry("generic ignores a directory rename on the wrong path", "generic", "plugin directory", "path"),
		Entry("PCI ignores the wrong kubelet operation", "PCI", "kubelet socket", "operation"),
		Entry("generic ignores the wrong kubelet operation", "generic", "kubelet socket", "operation"),
		Entry("PCI ignores the wrong plugin operation", "PCI", "plugin socket", "operation"),
		Entry("generic ignores the wrong plugin operation", "generic", "plugin socket", "operation"),
		Entry("PCI ignores the wrong directory operation", "PCI", "plugin directory", "operation"),
		Entry("generic ignores the wrong directory operation", "generic", "plugin directory", "operation"),
	)
})
