// Package collect gets raw event JSON from somewhere — a live cluster or a
// file on disk — and hands back the same bytes either way.
//
// Live collection marshals the API's EventList back to JSON and feeds it
// through the same decoder a `kubectl get events -A -o json` dump goes through.
// One code path, which is also why --dump-json can simply write those bytes out
// and --from-json can replay them later.
package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const pageSize = 500

// FromFile reads a previously captured payload. "-" means stdin.
func FromFile(path string) ([]byte, error) {
	if path == "-" {
		return readAll(os.Stdin)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

// Live lists events from the cluster the ambient kubeconfig points at. An empty
// namespace means all namespaces.
func Live(ctx context.Context, kubeconfig, kubecontext, namespace string, timeout time.Duration) ([]byte, error) {
	cfg, err := restConfig(kubeconfig, kubecontext)
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		cfg.Timeout = timeout
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}

	all := &corev1.EventList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EventList"}}
	opts := metav1.ListOptions{Limit: pageSize}
	for {
		page, err := cs.CoreV1().Events(namespace).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list events: %w", err)
		}
		all.Items = append(all.Items, page.Items...)
		if page.Continue == "" {
			break
		}
		opts.Continue = page.Continue
	}

	data, err := json.Marshal(all)
	if err != nil {
		return nil, fmt.Errorf("marshal events: %w", err)
	}
	return data, nil
}

func restConfig(kubeconfig, kubecontext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubecontext != "" {
		overrides.CurrentContext = kubecontext
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err == nil {
		return cfg, nil
	}
	// No usable kubeconfig: the binary may be running inside the cluster.
	if inCluster, inErr := rest.InClusterConfig(); inErr == nil {
		return inCluster, nil
	}
	return nil, fmt.Errorf("no usable kubeconfig and not running in-cluster: %w", err)
}
