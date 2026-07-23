package util

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	gateway "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"

	direwolf "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned"
)

func GetKubernetesClients() (
	clientset kubernetes.Interface,
	direwolfClient direwolf.Interface,
	gatewayClient gateway.Interface,
	dynamicClient dynamic.Interface,
	err error,
) {
	kubeConfig := os.Getenv("KUBECONFIG")
	if kubeConfig == "" {
		defaultPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		// TODO: finish linting and move this to in-cluster config
		if _, statErr := os.Stat(defaultPath); statErr == nil {
			kubeConfig = defaultPath
		}
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error building kubeconfig: %w", err)
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error creating clientset: %w", err)
	}

	dynamicClient, err = dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error creating dynamic client: %w", err)
	}

	direwolfClient, err = direwolf.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error creating direwolf client: %w", err)
	}

	gatewayClient, err = gateway.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error creating gateway client: %w", err)
	}

	return clientset, direwolfClient, gatewayClient, dynamicClient, nil
}
