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
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/pager"

	"github.com/sairohithg/kubectl-why-pending/pkg/diagnose"
)

const usage = `kubectl why-pending — explain why pods are stuck Pending

USAGE:
  kubectl why-pending [pod] [flags]

  With no pod name, diagnoses every Pending pod in the namespace.

FLAGS:
  -n, --namespace <ns>     namespace (default: current context namespace)
  -A, --all-namespaces     scan Pending pods in all namespaces
  -o, --output <format>    output format: text (default), json, or yaml
      --context <name>     kubeconfig context to use
      --kubeconfig <path>  path to kubeconfig
      --no-color           disable colored output
  -h, --help               show this help

EXIT CODES:
  0  no blocking cause found (or no Pending pods)
  1  at least one Pending pod has a blocking cause
  2  usage error
  3  runtime error (e.g. could not reach the cluster)
`

type options struct {
	namespace     string
	allNamespaces bool
	context       string
	kubeconfig    string
	podName       string
	noColor       bool
	output        string // "", "text", "json", "yaml"
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprint(os.Stderr, "\n"+usage)
		os.Exit(2)
	}
	switch opts.output {
	case "", "text", "json", "yaml":
	default:
		fmt.Fprintf(os.Stderr, "error: invalid --output %q (want text, json, or yaml)\n", opts.output)
		os.Exit(2)
	}

	client, defaultNS, err := newClient(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: could not connect to cluster:", err)
		os.Exit(3)
	}
	if opts.namespace == "" {
		opts.namespace = defaultNS
	}

	blockers, err := run(client, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(3)
	}
	if blockers {
		os.Exit(1) // pending pods with a blocking cause — scriptable signal
	}
}

// run diagnoses the pending pods and returns whether any has a blocking cause.
func run(client kubernetes.Interface, opts options) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pending, err := pendingPods(ctx, client, opts)
	if err != nil {
		return false, err
	}
	if len(pending) == 0 {
		if structured(opts.output) {
			return false, emit(opts.output, []diagnose.Result{})
		}
		scope := "namespace " + opts.namespace
		if opts.allNamespaces {
			scope = "any namespace"
		}
		fmt.Printf("No Pending pods found in %s. 🎉\n", scope)
		return false, nil
	}

	// Gather cluster-wide state once and reuse it for every pod.
	nodeViews, clusterPods, chain, err := gatherNodeViews(ctx, client)
	if err != nil {
		return false, err
	}
	draClaims, draSlices, draClasses := gatherDRA(ctx, client, pending)

	results := make([]diagnose.Result, 0, len(pending))
	for i := range pending {
		p := &pending[i]
		missing, unbound := unboundPVCs(ctx, client, p)
		in := diagnose.Input{
			Pod:            p,
			Nodes:          nodeViews,
			ClusterPods:    clusterPods,
			SchedulerEvent: latestSchedulerEvent(ctx, client, p),
			MissingPVCs:    missing,
			UnboundPVCs:    unbound,
			Chain:          &chain,
			DRAClaims:      draClaims,
			DRASlices:      draSlices,
			DRAClasses:     draClasses,
		}
		results = append(results, diagnose.Analyze(in))
	}

	if structured(opts.output) {
		if err := emit(opts.output, results); err != nil {
			return false, err
		}
	} else {
		w := newWriter(opts.noColor)
		for _, r := range results {
			w.report(r)
		}
	}

	blockers := false
	for _, r := range results {
		if r.HasBlocker() {
			blockers = true
			break
		}
	}
	return blockers, nil
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
		if p.Status.Phase != corev1.PodPending {
			fmt.Fprintf(os.Stderr, "Pod %s/%s is %s, not Pending — nothing to diagnose.\n", p.Namespace, p.Name, p.Status.Phase)
			return nil, nil
		}
		if p.Spec.NodeName != "" {
			fmt.Fprintf(os.Stderr, "Pod %s/%s is already scheduled to node %q (Pending on the kubelet, not the scheduler) — nothing to diagnose.\n", p.Namespace, p.Name, p.Spec.NodeName)
			return nil, nil
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

// podListPageSize bounds how many pods are held in memory at once while
// streaming the cluster-wide pod list.
const podListPageSize = 500

// gatherNodeViews lists nodes and sums the requests of pods already placed on
// each (for free-capacity math), and returns the placed pods (for topology /
// affinity analysis) plus the GPU enablement-chain status (for GPU diagnoses).
//
// The cluster-wide pod list is served from the apiserver watch cache
// (ResourceVersion "0") and streamed in bounded pages, so the full set of pod
// specs is never materialized at once — memory scales with page size, not total
// pod count.
func gatherNodeViews(ctx context.Context, client kubernetes.Interface) ([]diagnose.NodeView, []diagnose.PlacedPod, diagnose.ChainStatus, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return nil, nil, diagnose.ChainStatus{}, err
	}

	listPods := func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return client.CoreV1().Pods("").List(ctx, opts)
	}
	used, placed, chainPods, err := streamPods(ctx, listPods)
	if err != nil {
		return nil, nil, diagnose.ChainStatus{}, err
	}
	chain := diagnose.AnalyzeOperatorChain(chainPods)

	views := make([]diagnose.NodeView, 0, len(nodes.Items))
	for i := range nodes.Items {
		n := &nodes.Items[i]
		views = append(views, diagnose.NodeView{Node: n, Used: used[n.Name]})
	}
	return views, placed, chain, nil
}

// streamPods pages through every pod via listFn (served from the watch cache)
// and folds each one incrementally into the per-node request totals (used), the
// placed-pod summaries (placed), and the GPU operator-relevant subset
// (chainPods). No page beyond podListPageSize is held at once, and only the
// compact per-pod summaries — not full pod specs — are retained across pages.
func streamPods(ctx context.Context, listFn pager.ListPageFunc) (map[string]diagnose.Resources, []diagnose.PlacedPod, []corev1.Pod, error) {
	used := map[string]diagnose.Resources{}
	var placed []diagnose.PlacedPod
	var chainPods []corev1.Pod

	pgr := pager.New(listFn)
	pgr.PageSize = podListPageSize
	err := pgr.EachListItem(ctx, metav1.ListOptions{ResourceVersion: "0"}, func(obj runtime.Object) error {
		p, ok := obj.(*corev1.Pod)
		if !ok {
			return nil
		}
		if diagnose.ChainRelevant(p) {
			chainPods = append(chainPods, *p)
		}
		if p.Spec.NodeName == "" {
			return nil
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			return nil
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
		return nil
	})
	return used, placed, chainPods, err
}

// gatherDRA fetches the Dynamic Resource Allocation objects needed to diagnose
// pods that request devices via resourceClaims. It only hits the resource.k8s.io
// API when at least one pending pod uses DRA, and tolerates the API being absent
// (pre-1.34 clusters) or unreadable by returning whatever it could fetch.
func gatherDRA(ctx context.Context, client kubernetes.Interface, pending []corev1.Pod) ([]resourcev1.ResourceClaim, []resourcev1.ResourceSlice, []resourcev1.DeviceClass) {
	needed := false
	for i := range pending {
		if diagnose.UsesDRA(&pending[i]) {
			needed = true
			break
		}
	}
	if !needed {
		return nil, nil, nil
	}

	var claims []resourcev1.ResourceClaim
	var slices []resourcev1.ResourceSlice
	var classes []resourcev1.DeviceClass
	if cl, err := client.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{}); err == nil {
		claims = cl.Items
	}
	if sl, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{}); err == nil {
		slices = sl.Items
	}
	if cc, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{}); err == nil {
		classes = cc.Items
	}
	return claims, slices, classes
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

func unboundPVCs(ctx context.Context, client kubernetes.Interface, p *corev1.Pod) (missing, unbound []string) {
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		name := v.PersistentVolumeClaim.ClaimName
		pvc, err := client.CoreV1().PersistentVolumeClaims(p.Namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			missing = append(missing, name)
		case err != nil:
			// Some other error (RBAC, transient) — don't mislabel it as missing.
		case pvc.Status.Phase != corev1.ClaimBound:
			unbound = append(unbound, name)
		}
	}
	return
}
