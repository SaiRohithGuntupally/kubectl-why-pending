// kubectl-why-pending explains, in plain English, why pods are stuck Pending —
// with on-prem/bare-metal causes (no autoscaler, resource fragmentation,
// control-plane taints) called out specifically.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/pager"

	"github.com/SaiRohithGuntupally/kubectl-why-pending/pkg/diagnose"
)

// version is stamped at release time: -ldflags "-X main.version=v0.7.0".
var version = "dev"

type options struct {
	namespace     string // resolved: -n, else the kubeconfig context's namespace
	allNamespaces bool
	podName       string
	noColor       bool
	output        string        // "", "text", "json", "yaml"
	timeout       time.Duration // whole-run deadline (0 in tests = default)
	flags         *genericclioptions.ConfigFlags
}

func main() {
	opts, err := parseArgs(os.Args[1:], os.Stdout)
	switch {
	case errors.Is(err, errHelp), errors.Is(err, errVersion):
		os.Exit(0)
	case err != nil:
		printUsageError(err)
		os.Exit(2)
	}

	client, defaultNS, err := newClient(opts.flags)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: could not connect to cluster:", err)
		os.Exit(3)
	}
	if opts.namespace == "" {
		opts.namespace = defaultNS
	}

	blockers, err := run(client, opts)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w (raise --timeout, currently %s)", err, opts.timeout)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(3)
	}
	if blockers {
		os.Exit(1) // pending pods with a blocking cause — scriptable signal
	}
}

// run diagnoses the pending pods and returns whether any has a blocking cause.
func run(client kubernetes.Interface, opts options) (bool, error) {
	timeout := opts.timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pending, err := pendingPods(ctx, client, opts)
	if err != nil {
		return false, err
	}
	if len(pending) == 0 {
		if structured(opts.output) {
			return false, emit(opts.output, []diagnose.Result{})
		}
		if opts.podName != "" {
			return false, nil // pendingPods already said why (e.g. "is Running")
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
	draClaims, draSlices, draClasses, draTaintRules, draClaimErrs := gatherDRA(ctx, client, pending)
	classModes := map[string]storagev1.VolumeBindingMode{}

	results := make([]diagnose.Result, 0, len(pending))
	for i := range pending {
		p := &pending[i]
		var missing, unbound, waiting, unknownPVCs, readErrs []string
		if p.Spec.NodeName == "" {
			missing, unbound, waiting, unknownPVCs, readErrs = unboundPVCs(ctx, client, p, classModes)
		}
		event, err := latestSchedulerEvent(ctx, client, p)
		if err != nil {
			readErrs = append(readErrs, "events: "+err.Error())
		}
		in := diagnose.Input{
			Pod:            p,
			Nodes:          nodeViews,
			ClusterPods:    clusterPods,
			SchedulerEvent: event,
			ReadErrors:     readErrs,
			MissingPVCs:    missing,
			UnboundPVCs:    unbound,
			WaitingPVCs:    waiting,
			UnknownPVCs:    unknownPVCs,
			DRAClaimsError: draClaimErrs[p.Namespace],
			Chain:          &chain,
			DRAClaims:      draClaims,
			DRASlices:      draSlices,
			DRAClasses:     draClasses,
			DRATaintRules:  draTaintRules,
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

// newClient builds the clientset from the standard kubectl flags (kubeconfig,
// context, impersonation, token, server, request timeout, TLS) and returns the
// context's namespace as the default.
func newClient(flags *genericclioptions.ConfigFlags) (kubernetes.Interface, string, error) {
	ns, _, err := flags.ToRawKubeConfigLoader().Namespace()
	if err != nil || ns == "" {
		ns = "default"
	}
	cfg, err := flags.ToRESTConfig()
	if err != nil {
		return nil, "", err
	}
	cfg.UserAgent = "kubectl-why-pending/" + version
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
		// A Pending pod that already has a node is still reported (the engine
		// explains it is stuck on the kubelet), never silently dropped.
		return []corev1.Pod{*p}, nil
	}
	list, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Pending",
	})
	if err != nil {
		return nil, err
	}
	// Re-check the phase client-side so we don't rely solely on the server
	// field selector. Pods already bound to a node stay in: they show as Pending
	// in `kubectl get pods`, and the engine says they're stuck on the kubelet.
	var out []corev1.Pod
	for _, p := range list.Items {
		if p.Status.Phase == corev1.PodPending {
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
		addUsage(used, p)
		placed = append(placed, placedPod(p))
		return nil
	})
	return used, placed, chainPods, err
}

// addUsage folds a scheduled pod's requests (and its pod slot) into the
// per-node usage totals.
func addUsage(used map[string]diagnose.Resources, p *corev1.Pod) {
	r := diagnose.PodRequests(p)
	cur := used[p.Spec.NodeName]
	cur.CPUMilli += r.CPUMilli
	cur.MemBytes += r.MemBytes
	cur.EphemeralBytes += r.EphemeralBytes
	cur.Pods++
	if cur.Extended == nil {
		cur.Extended = map[string]int64{}
	}
	for name, v := range r.Extended {
		cur.Extended[name] += v
	}
	used[p.Spec.NodeName] = cur
}

// placedPod keeps only what the cross-pod analyses need from a scheduled pod.
func placedPod(p *corev1.Pod) diagnose.PlacedPod {
	pp := diagnose.PlacedPod{
		Namespace:   p.Namespace,
		Name:        p.Name,
		NodeName:    p.Spec.NodeName,
		Labels:      p.Labels,
		Terminating: p.DeletionTimestamp != nil,
	}
	if a := p.Spec.Affinity; a != nil && a.PodAntiAffinity != nil {
		pp.RequiredAntiAffinity = a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	}
	return pp
}

// gatherDRA fetches the Dynamic Resource Allocation objects needed to diagnose
// pods that request devices via resourceClaims. It only hits the resource.k8s.io
// API when at least one pending pod uses DRA. Claims are listed per namespace
// (namespace-scoped users can't list cluster-wide), and a failed claim list is
// reported in claimErrs[namespace] so the engine says "unknown", never "not
// found". Slices and classes stay nil when unreadable, which the engine treats
// as "not checked".
func gatherDRA(ctx context.Context, client kubernetes.Interface, pending []corev1.Pod) (claims []resourcev1.ResourceClaim, slices []resourcev1.ResourceSlice, classes []resourcev1.DeviceClass, rules []resourcev1.DeviceTaintRule, claimErrs map[string]string) {
	namespaces := map[string]bool{}
	for i := range pending {
		if diagnose.UsesDRA(&pending[i]) && pending[i].Spec.NodeName == "" {
			namespaces[pending[i].Namespace] = true
		}
	}
	if len(namespaces) == 0 {
		return nil, nil, nil, nil, nil
	}

	claimErrs = map[string]string{}
	for ns := range namespaces {
		cl, err := client.ResourceV1().ResourceClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			claimErrs[ns] = err.Error()
			continue
		}
		claims = append(claims, cl.Items...)
	}
	// nil means "couldn't read"; a successful read with zero items must be a
	// non-nil empty slice, because an empty list often decodes to nil and the
	// engine would then skip the "no DeviceClass" / "no driver" hints.
	if sl, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{}); err == nil {
		slices = append([]resourcev1.ResourceSlice{}, sl.Items...)
	}
	if cc, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{}); err == nil {
		classes = append([]resourcev1.DeviceClass{}, cc.Items...)
	}
	return claims, slices, classes, gatherDeviceTaintRules(ctx, client), claimErrs
}

// gatherDeviceTaintRules lists DeviceTaintRules (KEP-5055). They are served from
// resource.k8s.io/v1 since 1.37 (GA); 1.36 clusters with the DRADeviceTaints
// gate on only have v1beta2, so fall back to that and convert. Anything older
// has no such resource at all, which yields nil.
func gatherDeviceTaintRules(ctx context.Context, client kubernetes.Interface) []resourcev1.DeviceTaintRule {
	if rl, err := client.ResourceV1().DeviceTaintRules().List(ctx, metav1.ListOptions{}); err == nil {
		return append([]resourcev1.DeviceTaintRule{}, rl.Items...)
	}
	rl, err := client.ResourceV1beta2().DeviceTaintRules().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	rules := make([]resourcev1.DeviceTaintRule, 0, len(rl.Items))
	for i := range rl.Items {
		r := &rl.Items[i]
		out := resourcev1.DeviceTaintRule{ObjectMeta: r.ObjectMeta}
		if s := r.Spec.DeviceSelector; s != nil {
			out.Spec.DeviceSelector = &resourcev1.DeviceTaintSelector{Driver: s.Driver, Pool: s.Pool, Device: s.Device}
		}
		out.Spec.Taint = resourcev1.DeviceTaint{
			Key:       r.Spec.Taint.Key,
			Value:     r.Spec.Taint.Value,
			Effect:    resourcev1.DeviceTaintEffect(r.Spec.Taint.Effect),
			TimeAdded: r.Spec.Taint.TimeAdded,
		}
		rules = append(rules, out)
	}
	return rules
}

// latestSchedulerEvent returns the newest FailedScheduling message for the pod.
// The scheduler records through events.k8s.io/v1, whose core/v1 view often has
// a zero lastTimestamp, so the time falls back through series.lastObservedTime,
// eventTime, lastTimestamp and firstTimestamp the way kubectl's printer does.
// Events for an older pod with the same name (StatefulSet recreation) are
// skipped by UID.
func latestSchedulerEvent(ctx context.Context, client kubernetes.Interface, p *corev1.Pod) (string, error) {
	selector := "involvedObject.name=" + p.Name + ",involvedObject.kind=Pod"
	if p.UID != "" {
		selector += ",involvedObject.uid=" + string(p.UID)
	}
	events, err := client.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		return "", err
	}
	var msg string
	var latest time.Time
	found := false
	for i := range events.Items {
		e := &events.Items[i]
		if e.Reason != "FailedScheduling" {
			continue
		}
		if e.InvolvedObject.Kind != "Pod" || e.InvolvedObject.Name != p.Name {
			continue
		}
		if p.UID != "" && e.InvolvedObject.UID != "" && e.InvolvedObject.UID != p.UID {
			continue
		}
		ts := eventTime(e)
		if !found || ts.After(latest) {
			found = true
			latest = ts
			msg = strings.TrimSpace(e.Message)
		}
	}
	return msg, nil
}

func eventTime(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.FirstTimestamp.IsZero():
		return e.FirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

// unboundPVCs classifies the pod's claims (including generic ephemeral volumes'
// generated "<pod>-<volume>" PVCs): missing, unbound with Immediate binding (a
// real blocker), or waiting on a WaitForFirstConsumer StorageClass (normal until
// the pod is scheduled). classModes caches StorageClass lookups across pods.
func unboundPVCs(ctx context.Context, client kubernetes.Interface, p *corev1.Pod, classModes map[string]storagev1.VolumeBindingMode) (missing, unbound, waiting, unknown, readErrs []string) {
	for _, v := range p.Spec.Volumes {
		var name string
		ephemeral := false
		switch {
		case v.PersistentVolumeClaim != nil:
			name = v.PersistentVolumeClaim.ClaimName
		case v.Ephemeral != nil:
			name = p.Name + "-" + v.Name
			ephemeral = true
		default:
			continue
		}
		pvc, err := client.CoreV1().PersistentVolumeClaims(p.Namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if !ephemeral { // the ephemeral controller creates it shortly
				missing = append(missing, name)
			}
		case err != nil:
			// RBAC or transient: report it, never mislabel it as missing.
			readErrs = append(readErrs, fmt.Sprintf("PVC %s: %v", name, err))
		case pvc.Status.Phase == corev1.ClaimBound:
		default:
			mode, err := bindingMode(ctx, client, pvc, classModes)
			switch {
			case err != nil:
				unknown = append(unknown, fmt.Sprintf("%s (%v)", name, err))
			case mode == storagev1.VolumeBindingWaitForFirstConsumer:
				waiting = append(waiting, name)
			default:
				unbound = append(unbound, name)
			}
		}
	}
	return
}

// bindingMode returns the PVC's StorageClass volumeBindingMode, resolving the
// cluster default class when storageClassName is unset. Unknown → Immediate,
// which keeps the old (Blocker) behaviour.
// bindingMode returns the PVC's StorageClass volumeBindingMode, or an error
// when the StorageClass couldn't be read (RBAC). A missing class is Immediate:
// nothing will ever provision it, which is a real blocker.
func bindingMode(ctx context.Context, client kubernetes.Interface, pvc *corev1.PersistentVolumeClaim, cache map[string]storagev1.VolumeBindingMode) (storagev1.VolumeBindingMode, error) {
	var class string
	switch {
	case pvc.Spec.StorageClassName != nil:
		class = *pvc.Spec.StorageClassName
	default:
		class = "\x00default"
	}
	if class == "" {
		return storagev1.VolumeBindingImmediate, nil // static binding, no class
	}
	if m, ok := cache[class]; ok {
		return m, nil
	}
	mode := storagev1.VolumeBindingImmediate
	if class == "\x00default" {
		list, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", fmt.Errorf("couldn't list StorageClasses to find the default: %w", err)
		}
		for i := range list.Items {
			sc := &list.Items[i]
			if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" && sc.VolumeBindingMode != nil {
				mode = *sc.VolumeBindingMode
				break
			}
		}
	} else {
		sc, err := client.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			// No such class: nothing provisions it, so Immediate (blocker) is right.
		case err != nil:
			return "", fmt.Errorf("couldn't read StorageClass %q: %w", class, err)
		case sc.VolumeBindingMode != nil:
			mode = *sc.VolumeBindingMode
		}
	}
	cache[class] = mode
	return mode, nil
}
