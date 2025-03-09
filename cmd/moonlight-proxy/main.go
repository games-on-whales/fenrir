package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/controllers"
	"games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/moonlight"
	"games-on-whales.github.io/direwolf/pkg/util"

	_ "embed"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

func main() {
	appContext, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	serverCertPath := flag.String("tls-cert", "server.crt", "Path to server cert")
	serverKeyPath := flag.String("tls-key", "server.key", "Path to server key")
	flag.Parse()

	tlsCert, err := util.LoadCertificates(*serverCertPath, *serverKeyPath)
	if err != nil {
		fmt.Println("Error loading certificates:", err)
		os.Exit(1)
	}

	k8sClient, direwolfClient, gatewayClient, dynamicClient, err := util.GetKubernetesClients()
	if err != nil {
		fmt.Println("Error getting Kubernetes clients:", err)
		os.Exit(1)
	}

	namespace := "default"
	// Get pod namespace from envar set by k8s if it exists
	if ns, ok := os.LookupEnv("POD_NAMESPACE"); ok {
		namespace = ns
	}

	// Just create all the informers and warm them up before starting anything
	// to keep things simple.
	//
	// !TODO: Eventually will want to respond to /livez before caches are warm.
	direwolfFactory := externalversions.NewSharedInformerFactoryWithOptions(direwolfClient, 15*time.Minute, externalversions.WithNamespace(namespace))
	pairingInformer := direwolfFactory.Direwolf().V1alpha1().Pairings().Informer()
	appInformer := direwolfFactory.Direwolf().V1alpha1().Apps().Informer()
	userInformer := direwolfFactory.Direwolf().V1alpha1().Users().Informer()
	sessionInformer := direwolfFactory.Direwolf().V1alpha1().Sessions().Informer()
	direwolfFactory.Start(appContext.Done())
	defer direwolfFactory.Shutdown()

	k8sFactory := informers.NewSharedInformerFactoryWithOptions(k8sClient, 15*time.Minute, informers.WithNamespace(namespace))
	deploymentInformer := k8sFactory.Apps().V1().Deployments().Informer()
	podInformer := k8sFactory.Core().V1().Pods().Informer()
	k8sFactory.Start(appContext.Done())
	defer k8sFactory.Shutdown()

	k8sFactory.WaitForCacheSync(appContext.Done())
	direwolfFactory.WaitForCacheSync(appContext.Done())

	pairingManager := moonlight.NewPairingManager(
		tlsCert,
		direwolfClient.DirewolfV1alpha1().Pairings(namespace),
	)

	restServer := moonlight.NewRESTServer(
		pairingManager,
		tlsCert,
		generic.NewLister[*direwolfv1alpha1.Pairing](pairingInformer.GetIndexer()).Namespaced(namespace),
		generic.NewLister[*direwolfv1alpha1.User](userInformer.GetIndexer()).Namespaced(namespace),
		generic.NewLister[*direwolfv1alpha1.App](appInformer.GetIndexer()).Namespaced(namespace),
		generic.NewLister[*direwolfv1alpha1.Session](sessionInformer.GetIndexer()).Namespaced(namespace),
		generic.NewLister[*v1.Pod](podInformer.GetIndexer()).Namespaced(namespace),
		k8sClient,
		dynamicClient,
		direwolfClient.DirewolfV1alpha1().Sessions(namespace),
	)

	// TODO: Respond unhealthy if session controller is not running
	// TODO: Also respond unhealthy if moonlight server is not running
	// TODO: Consider splitting session controller into separate controller that
	// runs our business logic loops, leaving moonlight to this binary.
	go func() {
		controllerContext, cancel := context.WithCancel(appContext)
		defer cancel()

		sessionController := controllers.NewSessionController(
			k8sClient,
			gatewayClient.GatewayV1alpha2().TCPRoutes(namespace),
			gatewayClient.GatewayV1alpha2().UDPRoutes(namespace),
			direwolfClient.DirewolfV1alpha1().Sessions(namespace),
			generic.NewInformer[*direwolfv1alpha1.Session](sessionInformer),
			generic.NewInformer[*direwolfv1alpha1.App](appInformer),
			generic.NewInformer[*direwolfv1alpha1.User](userInformer),
			generic.NewInformer[*appsv1.Deployment](deploymentInformer),
		)

		lock, err := resourcelock.New(
			resourcelock.LeasesResourceLock,
			namespace,
			"direwolf-controller",
			k8sClient.CoreV1(),
			k8sClient.CoordinationV1(),
			resourcelock.ResourceLockConfig{Identity: "direwolf-controller"},
		)
		if err != nil {
			panic(err)
		}

		leaderelection.RunOrDie(appContext, leaderelection.LeaderElectionConfig{
			Lock:          lock,
			LeaseDuration: 15 * time.Second,
			RenewDeadline: 10 * time.Second,
			RetryPeriod:   2 * time.Second,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					log.Println("started leading")
					err := sessionController.Run(controllerContext)
					if err != nil && !errors.Is(err, context.Canceled) {
						panic(err)
					}
				},
				OnStoppedLeading: func() {
					log.Println("stopped leading")
					appCancel()

					//!TODO: also consider killing the app after we move into
					// separate binary
					// should loop here?
				},
				OnNewLeader: func(identity string) {
					log.Printf("new leader: %s\n", identity)
				},
			},
		})
	}()
	go func() {
		defer appCancel()
		restServer.Run(appContext)
	}()

	<-appContext.Done()
	log.Println("Shutting down")
}
