package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"games-on-whales.github.io/direwolf/pkg/controllers"
	"games-on-whales.github.io/direwolf/pkg/dra"
	direwolf "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned"
	informers "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/util"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

func main() {
	driverName := getEnv("DRIVER_NAME", "wolf.dra.io")
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		klog.Fatal("NODE_NAME environment variable is required")
	}
	podUID := os.Getenv("POD_UID")
	podName := os.Getenv("POD_NAME")
	podNamespace := os.Getenv("POD_NAMESPACE")
	if podName == "" || podNamespace == "" {
		klog.Warning("POD_NAME or POD_NAMESPACE not set; ResourceSlice will not carry agent pod info")
	}
	socketsDir := getEnv("SOCKETS_DIR", "/var/run/wolf-sockets")
	wolfSockPath := getEnv("WOLF_SOCKET_PATH", "/var/run/wolf.sock")
	cdiDir := getEnv("CDI_DIR", "/var/run/cdi")
	maxLobbies := getEnvInt("MAX_LOBBIES", 10)
	// maximum time to wait for lobby creation
	queueTimeoutSec := getEnvInt("QUEUE_TIMEOUT_SECONDS", 30)
	enableSSE := getEnv("WOLF_DRA_ENABLE_SSE", "false") == "true"

	logLevel := getEnvInt("LOG_LEVEL", 2)

	tlsCertPath := getEnv("TLS_CERT", "server.crt")
	tlsKeyPath := getEnv("TLS_KEY", "server.key")

	// Set klog verbosity level based on LOG_LEVEL env var
	var level klog.Level
	if err := level.Set(strconv.Itoa(logLevel)); err != nil {
		klog.ErrorS(err, "Invalid LOG_LEVEL, using default")
	} else {
		klog.V(0).InfoS("Log level set", "level", logLevel)
	}

	klog.InfoS("Starting wolf-dra",
		"driverName", driverName,
		"nodeName", nodeName,
		"socketsDir", socketsDir,
		"wolfSockPath", wolfSockPath,
		"cdiDir", cdiDir,
		"maxLobbies", maxLobbies,
		"queueTimeout", queueTimeoutSec,
		"enableSSE", enableSSE,
		"logLevel", logLevel,
	)

	cert, err := util.LoadCertificates(tlsCertPath, tlsKeyPath)
	if err != nil {
		klog.Fatal("Failed to load certificates: ", err)
	}
	_ = cert

	if err := waitForWolfSock(wolfSockPath, 30*time.Second); err != nil {
		klog.Fatal("wolf.sock not available: ", err)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatal("Failed to get in-cluster config: ", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatal("Failed to create clientset: ", err)
	}

	// Create direwolf clientset for Session CRD informers (Phase 2)
	dwClient, err := direwolf.NewForConfig(cfg)
	if err != nil {
		klog.Fatal("Failed to create direwolf clientset: ", err)
	}

	queueTimeout := time.Duration(queueTimeoutSec) * time.Second
	driver, err := dra.NewDriver(
		driverName, nodeName, socketsDir, wolfSockPath, cdiDir,
		maxLobbies, queueTimeout, nil, cs, dwClient,
	)
	if err != nil {
		klog.Fatal("Failed to create driver: ", err)
	}

	// Use WithCancelCause so HandleError can trigger a graceful shutdown.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	driver.SetCancelFunc(cancel)

	// Reconcile state immediately at startup, before the plugin registers.
	// This reads existing CDI files and compares with Wolf's active lobbies
	// to ensure we don't drop active streams or recreate running pods.
	klog.Info("Reconciling state with Wolf...")
	driver.ReconcileWithWolf(ctx)
	klog.Info("Reconciliation complete.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		klog.InfoS("Shutting down", "signal", s)
		cancel(nil)
	}()
	factory := informers.NewSharedInformerFactory(dwClient, 0)
	sessionInformer := factory.Direwolf().V1alpha1().Sessions().Informer()
	sessionLister := factory.Direwolf().V1alpha1().Sessions().Lister()
	sessionWorkqueue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[string](),
	)

	sessionInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			driver.HandleSessionAdd(ctx, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			driver.HandleSessionUpdate(ctx, newObj)
		},
		DeleteFunc: func(obj any) {
			driver.HandleSessionDelete(ctx, obj)
		},
	})

	driver.SetSessionInformer(sessionInformer, sessionLister, sessionWorkqueue)
	go factory.Start(ctx.Done())

	pluginDir := filepath.Join(kubeletplugin.KubeletPluginsDir, driverName)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		klog.Fatal("Failed to create plugin directory: ", err)
	}

	opts := []kubeletplugin.Option{
		kubeletplugin.DriverName(driverName),
		kubeletplugin.KubeClient(cs),
		kubeletplugin.NodeName(nodeName),
		kubeletplugin.CDIDirectory(cdiDir),
	}
	if podUID != "" {
		opts = append(opts, kubeletplugin.RollingUpdate(types.UID(podUID)))
	}

	helper, err := kubeletplugin.Start(ctx, driver, opts...)
	if err != nil {
		klog.Fatal("Failed to start kubelet plugin: ", err)
	}
	internalIP, externalIP := driver.GetNodeIPs(ctx)
	klog.InfoS("Resolved node IPs for ResourceSlice",
		"internalIP", internalIP, "externalIP", externalIP,
		"agentPod", podNamespace+"/"+podName)

	// ---- consumable capacity pool ----
	driverResources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {
				Slices: []resourceslice.Slice{
					{
						// TODO: include more node identifiable information
						// instead of having the direwolf operator figure it out
						Devices: []resourceapi.Device{
							{
								Name:                     "lobby-pool",
								AllowMultipleAllocations: new(true),
								Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
									"slots": {Value: resource.MustParse(strconv.Itoa(maxLobbies))},
								},
								Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
									"wolf.dra.io/type":              {StringValue: new("lobby")},
									"wolf.dra.io/nodeInternalIP":    {StringValue: new(internalIP)},
									"wolf.dra.io/nodeExternalIP":    {StringValue: new(externalIP)},
									"wolf.dra.io/agentPodName":      {StringValue: new(podName)},
									"wolf.dra.io/agentPodNamespace": {StringValue: new(podNamespace)},
								},
							},
						},
					},
				},
			},
		},
	}

	if err := helper.PublishResources(ctx, driverResources); err != nil {
		klog.Fatal("Failed to publish resources: ", err)
	}
	klog.InfoS("Published lobby pool", "slots", maxLobbies)

	if enableSSE {
		go runSSE(ctx, wolfSockPath)
	}

	<-ctx.Done()

	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		klog.ErrorS(cause, "Driver stopped due to fatal error")
	}

	helper.Stop()
	klog.Info("wolf-dra stopped")
}

func runSSE(ctx context.Context, wolfSockPath string) {
	wolfClient := wolfapi.NewClient(
		"http://localhost",
		&http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", wolfSockPath)
				},
			},
		},
	)

	agent := controllers.NewAgent(wolfClient)
	if err := agent.Run(ctx); err != nil {
		klog.ErrorS(err, "SSE agent exited")
	}
}

func waitForWolfSock(path string, timeout time.Duration) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-deadline:
			return errors.New("timeout")
		case <-tick.C:
			if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
				var d net.Dialer
				c, err := d.DialContext(context.Background(), "unix", path)
				if err == nil {
					c.Close()
					return nil
				}
			}
		}
	}
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getEnvInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
