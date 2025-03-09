package main

import (
	"context"
	"flag"
	"os"
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
	appContext, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	serverCertPath := flag.String("tls-cert", "server.crt", "Path to server cert")
	serverKeyPath := flag.String("tls-key", "server.key", "Path to server key")
	port := flag.Int("port", 47989, "Port to listen on")
	securePort := flag.Int("secure-port", 47984, "Secure port to listen on")
	namespace := flag.String("namespace", os.Getenv("POD_NAMESPACE"), "Namespace to watch")
	klog.InitFlags(nil)
	flag.Parse()

	klog.Info("Starting moonlight-proxy")
	klog.Info("TLS Cert: ", *serverCertPath)
	klog.Info("TLS Key: ", *serverKeyPath)
	klog.Info("Port: ", *port)
	klog.Info("Secure Port: ", *securePort)
	klog.Info("Namespace: ", *namespace)

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
			Port:       *port,
			SecurePort: *securePort,
			Cert:       tlsCert,
		},
	)

	go func() {
		defer appCancel()
		restServer.Run(appContext)
	}()

	<-appContext.Done()
	klog.Info("Shutting down")
}
