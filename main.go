// kubectl-why-pending explains, in plain English, why pods are stuck Pending —
// with on-prem/bare-metal causes (no autoscaler, resource fragmentation,
// control-plane taints) called out specifically.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sairohithg/kubectl-why-pending/pkg/diagnose"
)

const usage = `kubectl why-pending — explain why pods are stuck Pending

USAGE:
  kubectl why-pending [pod] [flags]

  With no pod name, diagnoses every Pending pod in the namespace.

FLAGS:
  -n, --namespace <ns>     namespace (default: current context namespace)
  -A, --all-namespaces     scan Pending pods in all namespaces
      --context <name>     kubeconfig context to use
      --kubeconfig <path>  path to kubeconfig
      --no-color           disable colored output
  -h, --help               show this help
`

type options struct {
	namespace     string
	allNamespaces bool
	context       string
	kubeconfig    string
	podName       string
	noColor       bool
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprint(os.Stderr, "\n"+usage)
		os.Exit(2)
	}

	client, defaultNS, err := newClient(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: could not connect to cluster:", err)
		os.Exit(1)
	}
	if opts.namespace == "" {
		opts.namespace = defaultNS
	}

	if err := run(client, opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(client kubernetes.Interface, opts options) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pending, err := pendingPods(ctx, client, opts)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		scope := "namespace " + opts.namespace
		if opts.allNamespaces {
			scope = "any namespace"
		}
		fmt.Printf("No Pending pods found in %s. 🎉\n", scope)
		return nil
	}

	// Gather cluster-wide state once and reuse it for every pod.
	nodeViews, clusterPods, err := gatherNodeViews(ctx, client)
	if err != nil {
		return err
	}

	w := newWriter(opts.noColor)
	for i := range pending {
		p := &pending[i]
		in := diagnose.Input{
			Pod:            p,
			Nodes:          nodeViews,
			ClusterPods:    clusterPods,
			SchedulerEvent: latestSchedulerEvent(ctx, client, p),
			UnboundPVCs:    unboundPVCs(ctx, client, p),
		}
		w.report(diagnose.Analyze(in))
	}
	return nil
}

func newClient(opts options) (kubernetes.Interface, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.kubeconfig != "" {
		rules.ExplicitPath = opts.kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if opts.context != "" {
		overrides.CurrentContext = opts.context
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	ns, _, _ := cc.Namespace()
	if ns == "" {
		ns = "default"
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	return cs, ns, err
}

func pendingPods(ctx context.Context, client kubernetes.Interface, opts options) ([]corev1.Pod, error) {
	ns := opts.namespace
	if opts.allNamespaces {
		ns = ""
	}
	if opts.podName != "" {
		p, err := client.CoreV1().Pods(ns).Get(ctx, opts.podName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return []corev1.Pod{*p}, nil
	}
	list, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Pending",
	})
	if err != nil {
		return nil, err
	}
	// Keep only unscheduled Pending pods (re-checked client-side so we don't
	// rely solely on the server field selector).
	var out []corev1.Pod
	for _, p := range list.Items {
		if p.Status.Phase == corev1.PodPending && p.Spec.NodeName == "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// gatherNodeViews lists nodes and sums the requests of pods already placed on
// each (for free-capacity math), and returns the placed pods (for topology /
// affinity analysis).
func gatherNodeViews(ctx context.Context, client kubernetes.Interface) ([]diagnose.NodeView, []diagnose.PlacedPod, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	allPods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	used := map[string]diagnose.Resources{}
	var placed []diagnose.PlacedPod
	for i := range allPods.Items {
		p := &allPods.Items[i]
		if p.Spec.NodeName == "" {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		r := diagnose.PodRequests(p)
		cur := used[p.Spec.NodeName]
		cur.CPUMilli += r.CPUMilli
		cur.MemBytes += r.MemBytes
		if cur.Extended == nil {
			cur.Extended = map[string]int64{}
		}
		for name, v := range r.Extended {
			cur.Extended[name] += v
		}
		used[p.Spec.NodeName] = cur
		placed = append(placed, diagnose.PlacedPod{
			Namespace: p.Namespace,
			NodeName:  p.Spec.NodeName,
			Labels:    p.Labels,
		})
	}
	views := make([]diagnose.NodeView, 0, len(nodes.Items))
	for i := range nodes.Items {
		n := &nodes.Items[i]
		views = append(views, diagnose.NodeView{Node: n, Used: used[n.Name]})
	}
	return views, placed, nil
}

func latestSchedulerEvent(ctx context.Context, client kubernetes.Interface, p *corev1.Pod) string {
	events, err := client.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + p.Name + ",involvedObject.kind=Pod",
	})
	if err != nil {
		return ""
	}
	var msg string
	var latest time.Time
	for i := range events.Items {
		e := events.Items[i]
		if e.Reason != "FailedScheduling" {
			continue
		}
		if e.InvolvedObject.Kind != "Pod" || e.InvolvedObject.Name != p.Name {
			continue
		}
		ts := e.LastTimestamp.Time
		if ts.After(latest) {
			latest = ts
			msg = strings.TrimSpace(e.Message)
		}
	}
	return msg
}

func unboundPVCs(ctx context.Context, client kubernetes.Interface, p *corev1.Pod) []string {
	var unbound []string
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		name := v.PersistentVolumeClaim.ClaimName
		pvc, err := client.CoreV1().PersistentVolumeClaims(p.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil || pvc.Status.Phase != corev1.ClaimBound {
			unbound = append(unbound, name)
		}
	}
	return unbound
}
