package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nodeList is the shape `kubectl get nodes,pods -A -o json` prints: a v1 List
// whose items carry their own kind.
type nodeList struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Items      []any  `json:"items"`
}

// LiveNodes lists every node and every pod in the cluster and returns them as
// one List, the same bytes a `kubectl get nodes,pods -A -o json` dump holds.
// Pods are always listed across all namespaces: headroom needs every request
// on a node, whatever namespace it came from.
func LiveNodes(ctx context.Context, kubeconfig, kubecontext string, timeout time.Duration) ([]byte, error) {
	cfg, err := restConfig(kubeconfig, kubecontext)
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		cfg.Timeout = timeout
	}
	cs, err := newClient(cfg)
	if err != nil {
		return nil, err
	}

	out := nodeList{APIVersion: "v1", Kind: "List", Items: []any{}}
	opts := metav1.ListOptions{Limit: pageSize}
	for {
		page, err := cs.CoreV1().Nodes().List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list nodes: %w", err)
		}
		for i := range page.Items {
			// Typed list results leave TypeMeta empty on items; the decoder
			// dispatches on kind, so put it back.
			n := page.Items[i]
			n.APIVersion, n.Kind = "v1", "Node"
			out.Items = append(out.Items, n)
		}
		if page.Continue == "" {
			break
		}
		opts.Continue = page.Continue
	}

	opts = metav1.ListOptions{Limit: pageSize}
	for {
		page, err := cs.CoreV1().Pods(corev1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list pods: %w", err)
		}
		for i := range page.Items {
			p := page.Items[i]
			p.APIVersion, p.Kind = "v1", "Pod"
			out.Items = append(out.Items, p)
		}
		if page.Continue == "" {
			break
		}
		opts.Continue = page.Continue
	}

	data, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal nodes and pods: %w", err)
	}
	return data, nil
}
