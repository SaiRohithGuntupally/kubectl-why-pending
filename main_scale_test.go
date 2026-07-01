package main

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/sairohithg/kubectl-why-pending/pkg/diagnose"
)

// referenceFold reproduces the pre-streaming gatherNodeViews logic over a fully
// materialized pod slice, so the streaming path can be proven identical.
func referenceFold(pods []corev1.Pod) (map[string]diagnose.Resources, []diagnose.PlacedPod, diagnose.ChainStatus) {
	chain := diagnose.AnalyzeOperatorChain(pods)
	used := map[string]diagnose.Resources{}
	var placed []diagnose.PlacedPod
	for i := range pods {
		p := &pods[i]
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
	return used, placed, chain
}

func opPod(name, node, waiting string) corev1.Pod {
	cs := corev1.ContainerStatus{Name: "c", Ready: waiting == ""}
	if waiting != "" {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: waiting}
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gpu-operator"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

// TestStreamPods_LargeClusterPaginates builds a ~500-node / 20k-pod synthetic
// cluster and asserts the streamed fold is identical to the materialize-all
// reference, and that the pod list was actually paginated (many LIST pages).
func TestStreamPods_LargeClusterPaginates(t *testing.T) {
	const (
		nodes = 500
		count = 20000
	)
	cpu := resource.MustParse("100m")
	mem := resource.MustParse("128Mi")

	pods := make([]corev1.Pod, 0, count+2)
	for i := 0; i < count; i++ {
		p := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), Namespace: "default"},
			Spec: corev1.PodSpec{
				NodeName: fmt.Sprintf("node-%d", i%nodes),
				Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem},
				}}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		if i%50 == 0 { // some pods unplaced (must be skipped)
			p.Spec.NodeName = ""
		}
		if i%51 == 0 { // some pods terminal (must not hold capacity)
			p.Status.Phase = corev1.PodSucceeded
		}
		pods = append(pods, p)
	}
	// A GPU operator chain: healthy driver, broken device-plugin.
	pods = append(pods,
		opPod("nvidia-driver-daemonset-x", "node-2", ""),
		opPod("nvidia-device-plugin-daemonset-y", "node-2", "CrashLoopBackOff"),
	)

	// A paginating list function that honors Limit/Continue and counts pages.
	pages := 0
	listFn := func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		pages++
		start := 0
		if opts.Continue != "" {
			start, _ = strconv.Atoi(opts.Continue)
		}
		limit := int(opts.Limit)
		if limit <= 0 || start+limit > len(pods) {
			limit = len(pods) - start
		}
		end := start + limit
		cont := ""
		if end < len(pods) {
			cont = strconv.Itoa(end)
		}
		pl := &corev1.PodList{Items: pods[start:end]}
		pl.Continue = cont
		return pl, nil
	}

	used, placed, chainPods, err := streamPods(context.Background(), listFn)
	if err != nil {
		t.Fatalf("streamPods: %v", err)
	}

	// Pagination must actually have happened.
	wantPages := (len(pods) + podListPageSize - 1) / podListPageSize
	if pages != wantPages {
		t.Errorf("expected %d list pages (bounded by page size %d), got %d", wantPages, podListPageSize, pages)
	}
	if pages < 2 {
		t.Fatalf("pagination was not exercised (only %d page)", pages)
	}

	// The streamed fold must match the materialize-all reference exactly.
	wantUsed, wantPlaced, wantChain := referenceFold(pods)
	if !reflect.DeepEqual(used, wantUsed) {
		t.Errorf("used map differs from reference (%d vs %d nodes)", len(used), len(wantUsed))
	}
	if !reflect.DeepEqual(placed, wantPlaced) {
		t.Errorf("placed slice differs from reference (%d vs %d)", len(placed), len(wantPlaced))
	}
	gotChain := diagnose.AnalyzeOperatorChain(chainPods)
	if !reflect.DeepEqual(gotChain, wantChain) {
		t.Errorf("operator-chain differs from reference:\ngot  %+v\nwant %+v", gotChain, wantChain)
	}
	// Sanity: the broken device-plugin must surface, and only the tiny operator
	// subset should have been retained (not all 20k pods).
	if gotChain.FirstBroken == nil || gotChain.FirstBroken.Name != "device-plugin" {
		t.Errorf("expected device-plugin as first broken link, got %+v", gotChain.FirstBroken)
	}
	if len(chainPods) != 2 {
		t.Errorf("expected only 2 operator-relevant pods retained, got %d", len(chainPods))
	}
}
