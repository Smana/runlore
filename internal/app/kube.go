package app

import (
	"log/slog"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubeClientset builds a read-only clientset for pod-log access, or nil when no
// cluster is reachable (e.g. local runs without a kubeconfig).
func KubeClientset(log *slog.Logger) *kubernetes.Clientset {
	restCfg, err := RestConfig()
	if err != nil {
		return nil
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Warn("clientset unavailable; controller_logs disabled", "err", err)
		return nil
	}
	return cs
}

// RestConfig builds a Kubernetes REST config from in-cluster config, falling back
// to the local kubeconfig (respecting $KUBECONFIG, then ~/.kube/config).
func RestConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}
