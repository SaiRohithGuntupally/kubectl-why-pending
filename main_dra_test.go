package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	resourcev1beta2 "k8s.io/api/resource/v1beta2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/SaiRohithGuntupally/kubectl-why-pending/pkg/diagnose"
)

func draPendingPod() *corev1.Pod {
	claim := "claim-1"
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "train", Namespace: "default"},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: &claim}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func TestGatherDRA_FetchesDeviceTaintRules(t *testing.T) {
	drv := "gpu.example.com"
	client := fake.NewSimpleClientset(
		draPendingPod(),
		&resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim-1", Namespace: "default"}},
		&resourcev1.ResourceSlice{ObjectMeta: metav1.ObjectMeta{Name: "s1"}, Spec: resourcev1.ResourceSliceSpec{Driver: drv, Devices: []resourcev1.Device{{Name: "gpu-0"}}}},
		&resourcev1.DeviceTaintRule{
			ObjectMeta: metav1.ObjectMeta{Name: "drain"},
			Spec: resourcev1.DeviceTaintRuleSpec{
				DeviceSelector: &resourcev1.DeviceTaintSelector{Driver: &drv},
				Taint:          resourcev1.DeviceTaint{Key: "maintenance", Effect: resourcev1.DeviceTaintEffectNoSchedule},
			},
		},
	)
	claims, slices, _, rules, _ := gatherDRA(context.Background(), client, []corev1.Pod{*draPendingPod()})
	if len(claims) != 1 || len(slices) != 1 {
		t.Fatalf("claims=%d slices=%d, want 1/1", len(claims), len(slices))
	}
	if len(rules) != 1 || rules[0].Name != "drain" {
		t.Fatalf("want the DeviceTaintRule fetched, got %+v", rules)
	}
}

func TestGatherDRA_FallsBackToV1beta2DeviceTaintRules(t *testing.T) {
	// A 1.36 cluster with the DRADeviceTaints gate serves DeviceTaintRule only
	// from v1beta2. The v1 list fails there; the v1beta2 rule must be converted.
	drv := "gpu.example.com"
	pool := "node-3"
	client := fake.NewSimpleClientset(
		draPendingPod(),
		&resourcev1beta2.DeviceTaintRule{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy"},
			Spec: resourcev1beta2.DeviceTaintRuleSpec{
				DeviceSelector: &resourcev1beta2.DeviceTaintSelector{Driver: &drv, Pool: &pool},
				Taint:          resourcev1beta2.DeviceTaint{Key: "maintenance", Value: "fw", Effect: resourcev1beta2.DeviceTaintEffectNoExecute},
			},
		},
	)
	client.PrependReactor("list", "devicetaintrules", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Version == "v1" {
			return true, nil, errors.New("the server could not find the requested resource")
		}
		return false, nil, nil
	})
	_, _, _, rules, _ := gatherDRA(context.Background(), client, []corev1.Pod{*draPendingPod()})
	if len(rules) != 1 {
		t.Fatalf("want 1 converted rule, got %+v", rules)
	}
	r := rules[0]
	if r.Name != "legacy" || r.Spec.DeviceSelector == nil || *r.Spec.DeviceSelector.Driver != drv || *r.Spec.DeviceSelector.Pool != pool || r.Spec.DeviceSelector.Device != nil {
		t.Errorf("selector not converted faithfully: %+v", r.Spec.DeviceSelector)
	}
	if r.Spec.Taint.Key != "maintenance" || r.Spec.Taint.Value != "fw" || r.Spec.Taint.Effect != resourcev1.DeviceTaintEffectNoExecute {
		t.Errorf("taint not converted faithfully: %+v", r.Spec.Taint)
	}
}

func TestGatherDRA_ToleratesMissingDeviceTaintRuleAPI(t *testing.T) {
	// Pre-1.36 clusters don't serve DeviceTaintRule from any version; the other
	// DRA objects must still come back and rules must be nil.
	client := fake.NewSimpleClientset(
		draPendingPod(),
		&resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim-1", Namespace: "default"}},
	)
	client.PrependReactor("list", "devicetaintrules", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server could not find the requested resource")
	})
	claims, _, _, rules, _ := gatherDRA(context.Background(), client, []corev1.Pod{*draPendingPod()})
	if len(claims) != 1 {
		t.Fatalf("claims=%d, want 1", len(claims))
	}
	if rules != nil {
		t.Fatalf("want nil rules when the API is absent, got %+v", rules)
	}
}

func TestGatherDRA_SkipsWhenNoDRAPods(t *testing.T) {
	client := fake.NewSimpleClientset()
	calls := 0
	client.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		return false, nil, nil
	})
	plain := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	gatherDRA(context.Background(), client, []corev1.Pod{plain})
	if calls != 0 {
		t.Fatalf("no DRA pods: expected zero resource.k8s.io calls, got %d", calls)
	}
}

// Report 2.6 / scenario 17: a cluster with no DeviceClasses and no
// ResourceSlices (no DRA driver at all) must still produce the
// "DeviceClass not found" hint, so empty reads must not come back as nil.
func TestGatherDRA_EmptyListsAreNotNil(t *testing.T) {
	p := draPendingPod()
	client := fake.NewSimpleClientset(p, draClaimFor(p, "missing.example.com"))
	claims, slices, classes, _, errs := gatherDRA(context.Background(), client, []corev1.Pod{*p})
	if slices == nil || classes == nil {
		t.Fatalf("successful empty reads must be non-nil (slices=%v classes=%v)", slices == nil, classes == nil)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected read errors: %v", errs)
	}
	got := diagnose.AnalyzeDRA(p, claims, slices, classes, nil)
	if len(got) == 0 || !strings.Contains(got[0].Detail, "not found in the cluster") {
		t.Fatalf("want the missing-DeviceClass hint, got: %+v", got)
	}
}

func draClaimFor(p *corev1.Pod, class string) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: *p.Spec.ResourceClaims[0].ResourceClaimName, Namespace: p.Namespace},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{
			{Name: "gpu", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: class}},
		}}},
	}
}
