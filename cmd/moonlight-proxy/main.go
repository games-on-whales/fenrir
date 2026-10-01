package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/moonlight"
	"games-on-whales.github.io/direwolf/pkg/util"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/klog/v2"
)

func main() {
	// SIGTERM (pod stop) cancels the context so Run shuts the servers down
	// instead of the process dying mid-request.
	appContext, appCancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer appCancel()

	serverCertPath := flag.String("tls-cert", "server.crt", "Path to server cert")
	serverKeyPath := flag.String("tls-key", "server.key", "Path to server key")
	port := flag.Int("port", 47989, "Port to listen on")
	securePort := flag.Int("secure-port", 47984, "Secure port to listen on")
	launchTimeout := flag.Duration("launch-timeout", moonlight.DefaultLaunchTimeout, "How long to wait for a session to become ready when launching an app (must be under moonlight-qt's 120s launch timeout)")
	maxSessions := flag.Int("max-concurrent-sessions", 1, "How many users may stream at once; further launches get Moonlight's busy error (-1 = unlimited)")
	pinPort := flag.Int("pin-port", 0, "Port for the pairing page, to be reached only through the Authentik outpost. 0 disables it")
	pinTrustedProxies := flag.String("pin-trusted-proxies", "", "Comma-separated CIDRs of the proxy allowed to assert the user via --pin-user-header. Required with --pin-port")
	pinProxySecretFile := flag.String("pin-proxy-secret-file", "", "File holding the secret the proxy sends in "+moonlight.PinProxySecretHeader+". Required with --pin-port")
	pinUserHeader := flag.String("pin-user-header", moonlight.DefaultPinUserHeader, "Header carrying the authenticated username; must match a User name")
	namespace := flag.String("namespace", os.Getenv("POD_NAMESPACE"), "Namespace to watch")
	klog.InitFlags(nil)
	flag.Parse()

	if *launchTimeout <= 0 || *launchTimeout >= moonlight.ClientLaunchTimeout {
		klog.Fatalf("--launch-timeout must be > 0 and < %s (moonlight-qt's own launch timeout)", moonlight.ClientLaunchTimeout)
	}
	if *maxSessions == 0 || *maxSessions < -1 {
		klog.Fatal("--max-concurrent-sessions must be >= 1, or -1 for unlimited")
	}

	klog.Info("Starting moonlight-proxy")
	klog.Info("TLS Cert: ", *serverCertPath)
	klog.Info("TLS Key: ", *serverKeyPath)
	klog.Info("Port: ", *port)
	klog.Info("Secure Port: ", *securePort)
	klog.Info("Launch timeout: ", *launchTimeout)
	klog.Info("Max concurrent sessions: ", *maxSessions)
	klog.Info("Namespace: ", *namespace)

	trustedProxies, err := util.ParsePrefixes(*pinTrustedProxies)
	if err != nil {
		klog.Fatalf("--pin-trusted-proxies: %s", err)
	}
	if *pinPort != 0 && len(trustedProxies) == 0 {
		klog.Fatal("--pin-port requires --pin-trusted-proxies")
	}
	var proxySecret []byte
	if *pinPort != 0 {
		if *pinProxySecretFile == "" {
			klog.Fatal("--pin-port requires --pin-proxy-secret-file")
		}
		raw, readErr := os.ReadFile(*pinProxySecretFile)
		if readErr != nil {
			klog.Fatalf("--pin-proxy-secret-file: %s", readErr)
		}
		proxySecret = bytes.TrimSpace(raw)
		if len(proxySecret) < moonlight.MinPinProxySecretLen {
			klog.Fatalf("--pin-proxy-secret-file: secret is %d bytes, want at least %d", len(proxySecret), moonlight.MinPinProxySecretLen)
		}
	}
	klog.Info("Pairing page port: ", *pinPort)
	klog.Info("Pairing page trusted proxies: ", trustedProxies)

	tlsCert, err := util.LoadCertificates(*serverCertPath, *serverKeyPath)
	if err != nil {
		klog.Fatal("Failed to load certificates:", err)
	}

	k8sClient, direwolfClient, _, _, err := util.GetKubernetesClients()
	if err != nil {
		klog.Fatal("Error getting Kubernetes clients", err)
	}

	direwolfFactory := externalversions.NewSharedInformerFactoryWithOptions(
		direwolfClient,
		15*time.Minute,
		externalversions.WithNamespace(*namespace),
	)
	pairingInformer := direwolfFactory.Direwolf().V1alpha1().Pairings().Informer()
	appInformer := direwolfFactory.Direwolf().V1alpha1().Apps().Informer()
	userInformer := direwolfFactory.Direwolf().V1alpha1().Users().Informer()
	sessionInformer := direwolfFactory.Direwolf().V1alpha1().Sessions().Informer()
	direwolfFactory.Start(appContext.Done())
	defer direwolfFactory.Shutdown()

	k8sFactory := informers.NewSharedInformerFactoryWithOptions(k8sClient, 15*time.Minute, informers.WithNamespace(*namespace))
	podInformer := k8sFactory.Core().V1().Pods().Informer()
	k8sFactory.Start(appContext.Done())
	defer k8sFactory.Shutdown()

	// !TODO: Eventually will want to respond to /livez before caches are warm.
	klog.Info("Waiting for caches to sync")
	k8sFactory.WaitForCacheSync(appContext.Done())
	direwolfFactory.WaitForCacheSync(appContext.Done())
	if appContext.Err() != nil {
		klog.Info("Shutting down before caches synced")
		return
	}
	klog.Info("Caches synced")

	pairingManager := moonlight.NewPairingManager(
		tlsCert,
		direwolfClient.DirewolfV1alpha1().Pairings(*namespace),
	)

	restServer := moonlight.NewRESTServer(
		pairingManager,
		generic.NewLister[*direwolfv1alpha1.Pairing](pairingInformer.GetIndexer()).Namespaced(*namespace),
		generic.NewLister[*direwolfv1alpha1.User](userInformer.GetIndexer()).Namespaced(*namespace),
		generic.NewLister[*direwolfv1alpha1.App](appInformer.GetIndexer()).Namespaced(*namespace),
		generic.NewLister[*direwolfv1alpha1.Session](sessionInformer.GetIndexer()).Namespaced(*namespace),
		generic.NewLister[*v1.Pod](podInformer.GetIndexer()).Namespaced(*namespace),
		direwolfClient.DirewolfV1alpha1().Sessions(*namespace),
		moonlight.RESTServerOptions{
			Port:                  *port,
			SecurePort:            *securePort,
			Cert:                  tlsCert,
			LaunchTimeout:         *launchTimeout,
			MaxConcurrentSessions: *maxSessions,
			BusyCheck:             moonlight.LibraryBusyCheck(k8sClient.CoreV1().Pods(*namespace)),
			PinPage: moonlight.PinPageOptions{
				Port:           *pinPort,
				TrustedProxies: trustedProxies,
				ProxySecret:    proxySecret,
				UserHeader:     *pinUserHeader,
			},
		},
	)

	// Run returns once its servers are shut down; its graceful shutdown is
	// bounded, so this cannot hang the pod stop.
	if err := restServer.Run(appContext); err != nil {
		klog.Fatalf("Server failed: %s", err)
	}
	klog.Info("Shut down")
}
