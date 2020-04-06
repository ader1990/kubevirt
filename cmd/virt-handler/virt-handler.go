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
 * Copyright 2017 Red Hat, Inc.
 *
 */

package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/emicklei/go-restful"
	"github.com/golang/glog"
	flag "github.com/spf13/pflag"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/scheme"
	k8coresv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/certificate"

	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"
	clientutil "kubevirt.io/client-go/util"
	"kubevirt.io/kubevirt/pkg/certificates/bootstrap"
	"kubevirt.io/kubevirt/pkg/controller"
	inotifyinformer "kubevirt.io/kubevirt/pkg/inotify-informer"
	_ "kubevirt.io/kubevirt/pkg/monitoring/client/prometheus"    // import for prometheus metrics
	_ "kubevirt.io/kubevirt/pkg/monitoring/reflector/prometheus" // import for prometheus metrics
	promvm "kubevirt.io/kubevirt/pkg/monitoring/vms/prometheus"  // import for prometheus metrics
	_ "kubevirt.io/kubevirt/pkg/monitoring/workqueue/prometheus" // import for prometheus metrics
	"kubevirt.io/kubevirt/pkg/service"
	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/util/webhooks"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
	virthandler "kubevirt.io/kubevirt/pkg/virt-handler"
	virtcache "kubevirt.io/kubevirt/pkg/virt-handler/cache"
	"kubevirt.io/kubevirt/pkg/virt-handler/isolation"
	"kubevirt.io/kubevirt/pkg/virt-handler/rest"
	"kubevirt.io/kubevirt/pkg/virt-handler/selinux"
	virtlauncher "kubevirt.io/kubevirt/pkg/virt-launcher"
	virt_api "kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

const (
	defaultWatchdogTimeout = 15 * time.Second

	// Default port that virt-handler listens on.
	defaultPort = 8185

	// Default address that virt-handler listens on.
	defaultHost = "0.0.0.0"

	hostOverride = ""

	podIpAddress = ""

	// This value is derived from default MaxPods in Kubelet Config
	maxDevices = 110

	maxRequestsInFlight = 3
	// Default port that virt-handler listens to console requests
	defaultConsoleServerPort = 8186
)

type virtHandlerApp struct {
	service.ServiceListen
	HostOverride            string
	PodIpAddress            string
	VirtShareDir            string
	VirtLibDir              string
	WatchdogTimeoutDuration time.Duration
	MaxDevices              int
	MaxRequestsInFlight     int

	virtCli   kubecli.KubevirtClient
	namespace string

	serverTLSConfig   *tls.Config
	clientTLSConfig   *tls.Config
	consoleServerPort int
	clientcertmanager certificate.Manager
	servercertmanager certificate.Manager
	promTLSConfig     *tls.Config
}

var _ service.Service = &virtHandlerApp{}

func (app *virtHandlerApp) prepareCertManager() (err error) {
	app.clientcertmanager = bootstrap.NewFileCertificateManager("/etc/virt-handler/clientcertificates")
	app.servercertmanager = bootstrap.NewFileCertificateManager("/etc/virt-handler/servercertificates")
	return
}

func (app *virtHandlerApp) Run() {
	// HostOverride should default to os.Hostname(), to make sure we handle errors ensure it here.
	if app.HostOverride == "" {
		defaultHostName, err := os.Hostname()
		if err != nil {
			panic(err)
		}
		app.HostOverride = defaultHostName
	}

	if app.PodIpAddress == "" {
		panic(fmt.Errorf("no pod ip detected"))
	}

	logger := log.Log
	logger.V(1).Level(log.INFO).Log("hostname", app.HostOverride)
	var err error

	// Copy container-disk binary
	targetFile := filepath.Join(app.VirtLibDir, "/init/usr/bin/container-disk")
	err = os.MkdirAll(filepath.Dir(targetFile), os.ModePerm)
	if err != nil {
		panic(err)
	}
	err = copy("/usr/bin/container-disk", targetFile)
	if err != nil {
		panic(err)
	}

	// Create event recorder
	app.virtCli, err = kubecli.GetKubevirtClient()
	if err != nil {
		panic(err)
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&k8coresv1.EventSinkImpl{Interface: app.virtCli.CoreV1().Events(k8sv1.NamespaceAll)})
	// Scheme is used to create an ObjectReference from an Object (e.g. VirtualMachineInstance) during Event creation
	recorder := broadcaster.NewRecorder(scheme.Scheme, k8sv1.EventSource{Component: "virt-handler", Host: app.HostOverride})

	vmiSourceLabel, err := labels.Parse(fmt.Sprintf(v1.NodeNameLabel+" in (%s)", app.HostOverride))
	if err != nil {
		panic(err)
	}
	vmiTargetLabel, err := labels.Parse(fmt.Sprintf(v1.MigrationTargetNodeNameLabel+" in (%s)", app.HostOverride))
	if err != nil {
		panic(err)
	}

	// Wire VirtualMachineInstance controller

	vmSourceSharedInformer := cache.NewSharedIndexInformer(
		controller.NewListWatchFromClient(app.virtCli.RestClient(), "virtualmachineinstances", k8sv1.NamespaceAll, fields.Everything(), vmiSourceLabel),
		&v1.VirtualMachineInstance{},
		0,
		cache.Indexers{},
	)

	vmTargetSharedInformer := cache.NewSharedIndexInformer(
		controller.NewListWatchFromClient(app.virtCli.RestClient(), "virtualmachineinstances", k8sv1.NamespaceAll, fields.Everything(), vmiTargetLabel),
		&v1.VirtualMachineInstance{},
		0,
		cache.Indexers{},
	)

	// Wire Domain controller
	domainSharedInformer, err := virtcache.NewSharedInformer(app.VirtShareDir, int(app.WatchdogTimeoutDuration.Seconds()), recorder, vmSourceSharedInformer.GetStore())
	if err != nil {
		panic(err)
	}

	virtlauncher.InitializeSharedDirectories(app.VirtShareDir)

	app.namespace, err = clientutil.GetNamespace()
	if err != nil {
		glog.Fatalf("Error searching for namespace: %v", err)
	}

	if err := app.prepareCertManager(); err != nil {
		glog.Fatalf("Error preparing the certificate manager: %v", err)
	}

	factory := controller.NewKubeInformerFactory(app.virtCli.RestClient(), app.virtCli, nil, app.namespace)

	if err := app.setupTLS(factory); err != nil {
		glog.Fatalf("Error constructing migration tls config: %v", err)
	}

	gracefulShutdownInformer := cache.NewSharedIndexInformer(
		inotifyinformer.NewFileListWatchFromClient(
			virtlauncher.GracefulShutdownTriggerDir(app.VirtShareDir)),
		&virt_api.Domain{},
		0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

	podIsolationDetector := isolation.NewSocketBasedIsolationDetector(app.VirtShareDir)
	vmiInformer := factory.VMI()
	clusterConfig := virtconfig.NewClusterConfig(factory.ConfigMap(), factory.CRD(), app.namespace)

	vmController := virthandler.NewController(
		recorder,
		app.virtCli,
		app.HostOverride,
		app.PodIpAddress,
		app.VirtShareDir,
		vmSourceSharedInformer,
		vmTargetSharedInformer,
		domainSharedInformer,
		gracefulShutdownInformer,
		int(app.WatchdogTimeoutDuration.Seconds()),
		app.MaxDevices,
		clusterConfig,
		app.serverTLSConfig,
		app.clientTLSConfig,
		podIsolationDetector,
	)

	consoleHandler := rest.NewConsoleHandler(
		podIsolationDetector,
		vmiInformer,
	)

	lifecycleHandler := rest.NewLifecycleHandler(
		vmiInformer,
		app.VirtShareDir,
	)

	promvm.SetupCollector(app.virtCli, app.VirtShareDir, app.HostOverride, app.MaxRequestsInFlight)

	go app.clientcertmanager.Start()
	go app.servercertmanager.Start()

	// Bootstrapping. From here on the startup order matters
	stop := make(chan struct{})
	defer close(stop)
	factory.Start(stop)

	selinuxLauncherType := clusterConfig.GetSELinuxLauncherType()
	se, exists, err := selinux.NewSELinux()
	if err == nil && exists {
		for _, dir := range []string{app.VirtShareDir, app.VirtLibDir} {
			if labeled, err := se.IsLabeled(dir); err != nil {
				panic(err)
			} else if !labeled {
				err := se.Label("container_file_t", dir)
				if err != nil {
					panic(err)
				}
			}
			err := se.Restore(dir)
			if err != nil {
				panic(err)
			}
		}
		// Only install KubeVirt's policy if not using a custom one
		if selinuxLauncherType == "" {
			err = se.InstallPolicy("/var/run/kubevirt")
		}
		if err != nil {
			panic(fmt.Errorf("failed to install virt-launcher selinux policy: %v", err))
		}
	} else if err != nil {
		//an error occured
		panic(fmt.Errorf("failed to detect the presence of selinux: %v", err))
	}

	// Make sure the tun module is loaded on the node
	// Just opening and closing /dev/net/tun triggers a modprobe on the host if necessary
	devnettun, err := os.Open("/dev/net/tun")
	if err == nil {
		devnettun.Close()
	}

	cache.WaitForCacheSync(stop, factory.ConfigMap().HasSynced, vmiInformer.HasSynced, factory.CRD().HasSynced)

	go vmController.Run(10, stop)

	errCh := make(chan error)
	promErrCh := make(chan error)
	go app.runPrometheusServer(promErrCh)
	go app.runServer(errCh, consoleHandler, lifecycleHandler)

	// wait for one of the servers to exit
	fmt.Println(<-errCh)
}

func (app *virtHandlerApp) runPrometheusServer(errCh chan error) {
	log.Log.V(1).Infof("metrics: max concurrent requests=%d", app.MaxRequestsInFlight)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promvm.Handler(app.MaxRequestsInFlight))
	server := http.Server{
		Addr:      app.ServiceListen.Address(),
		Handler:   mux,
		TLSConfig: app.promTLSConfig,
	}
	errCh <- server.ListenAndServeTLS("", "")
}

func (app *virtHandlerApp) runServer(errCh chan error, consoleHandler *rest.ConsoleHandler, lifecycleHandler *rest.LifecycleHandler) {
	ws := new(restful.WebService)
	ws.Route(ws.GET("/v1/namespaces/{namespace}/virtualmachineinstances/{name}/console").To(consoleHandler.SerialHandler))
	ws.Route(ws.GET("/v1/namespaces/{namespace}/virtualmachineinstances/{name}/vnc").To(consoleHandler.VNCHandler))
	ws.Route(ws.PUT("/v1/namespaces/{namespace}/virtualmachineinstances/{name}/pause").To(lifecycleHandler.PauseHandler))
	ws.Route(ws.PUT("/v1/namespaces/{namespace}/virtualmachineinstances/{name}/unpause").To(lifecycleHandler.UnpauseHandler))
	ws.Route(ws.GET("/v1/namespaces/{namespace}/virtualmachineinstances/{name}/guestosinfo").To(lifecycleHandler.GetGuestInfo).Produces(restful.MIME_JSON).Consumes(restful.MIME_JSON).Returns(http.StatusOK, "OK", v1.VirtualMachineInstanceGuestAgentInfo{}))
	ws.Route(ws.GET("/v1/namespaces/{namespace}/virtualmachineinstances/{name}/userlist").To(lifecycleHandler.GetUsers).Produces(restful.MIME_JSON).Consumes(restful.MIME_JSON).Returns(http.StatusOK, "OK", v1.VirtualMachineInstanceGuestOSUserList{}))
	ws.Route(ws.GET("/v1/namespaces/{namespace}/virtualmachineinstances/{name}/filesystemlist").To(lifecycleHandler.GetFilesystems).Produces(restful.MIME_JSON).Consumes(restful.MIME_JSON).Returns(http.StatusOK, "OK", v1.VirtualMachineInstanceFileSystemList{}))
	restful.DefaultContainer.Add(ws)
	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", app.ServiceListen.BindAddress, app.consoleServerPort),
		Handler: restful.DefaultContainer,
		// we use migration TLS also for console connections (initiated by virt-api)
		TLSConfig: app.serverTLSConfig,
	}
	errCh <- server.ListenAndServeTLS("", "")
}

func (app *virtHandlerApp) AddFlags() {
	app.InitFlags()

	app.BindAddress = defaultHost
	app.Port = defaultPort

	app.AddCommonFlags()

	flag.StringVar(&app.HostOverride, "hostname-override", hostOverride,
		"Name under which the node is registered in Kubernetes, where this virt-handler instance is running on")

	flag.StringVar(&app.PodIpAddress, "pod-ip-address", podIpAddress,
		"The pod ip address")

	flag.StringVar(&app.VirtShareDir, "kubevirt-share-dir", util.VirtShareDir,
		"Shared directory between virt-handler and virt-launcher")

	flag.StringVar(&app.VirtLibDir, "kubevirt-lib-dir", util.VirtLibDir,
		"Shared lib directory between virt-handler and virt-launcher")

	flag.DurationVar(&app.WatchdogTimeoutDuration, "watchdog-timeout", defaultWatchdogTimeout,
		"Watchdog file timeout")

	// TODO: the Device Plugin API does not allow for infinitely available (shared) devices
	// so the current approach is to register an arbitrary number.
	// This should be deprecated if the API allows for shared resources in the future
	flag.IntVar(&app.MaxDevices, "max-devices", maxDevices,
		"Number of devices to register with Kubernetes device plugin framework")

	flag.IntVar(&app.MaxRequestsInFlight, "max-metric-requests", maxRequestsInFlight,
		"Number of concurrent requests to the metrics endpoint")

	flag.IntVar(&app.consoleServerPort, "console-server-port", defaultConsoleServerPort,
		"The port virt-handler listens on for console requests")
}

func (app *virtHandlerApp) setupTLS(factory controller.KubeInformerFactory) error {
	kubevirtCAConfigInformer := factory.KubeVirtCAConfigMap()
	caManager := webhooks.NewCAManager(kubevirtCAConfigInformer.GetStore(), app.namespace)

	app.promTLSConfig = webhooks.SetupPromTLS(app.servercertmanager)
	app.serverTLSConfig = webhooks.SetupTLSForVirtHandlerServer(caManager, app.servercertmanager)
	app.clientTLSConfig = webhooks.SetupTLSForVirtHandlerClients(caManager, app.clientcertmanager)

	return nil
}

func main() {
	app := &virtHandlerApp{}
	service.Setup(app)
	log.InitializeLogging("virt-handler")
	app.Run()
}

func copy(sourceFile string, targetFile string) error {

	if err := os.RemoveAll(targetFile); err != nil {
		return fmt.Errorf("failed to remove target file: %v", err)
	}
	target, err := os.Create(targetFile)
	if err != nil {
		return fmt.Errorf("failed to crate target file: %v", err)
	}
	defer target.Close()
	source, err := os.Open(sourceFile)
	if err != nil {
		return fmt.Errorf("failed to open source file: %v", err)
	}
	defer source.Close()
	_, err = io.Copy(target, source)
	if err != nil {
		return fmt.Errorf("failed to copy file: %v", err)
	}
	err = os.Chmod(targetFile, 0555)
	if err != nil {
		return fmt.Errorf("failed to make file executable: %v", err)
	}
	return nil
}
