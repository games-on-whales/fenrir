package util

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	direwolf "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned"
	gateway "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

func GetKubernetesClients() (
	kubernetes.Interface,
	direwolf.Interface,
	gateway.Interface,
	dynamic.Interface,
	error,
) {
	kubeConfig := os.Getenv("KUBECONFIG")
	if kubeConfig == "" {
		// Check exists before setting default
		defaultPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		if _, err := os.Stat(defaultPath); err == nil {
			kubeConfig = defaultPath
		}
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error building kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error creating clientset: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error creating dynamic client: %w", err)
	}

	versionedClient, err := direwolf.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error creating versioned client: %w", err)
	}

	gatewayClient, err := gateway.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error creating gateway client: %w", err)
	}

	return clientset, versionedClient, gatewayClient, dynamicClient, nil
}
