package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Audit risk 16: events recorded via events.k8s.io have a zero lastTimestamp;
// the message must still be found, and events for an older pod with the same
// name (different UID) must be ignored.
func TestLatestSchedulerEvent_FallbacksAndUID(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default", UID: "new-uid"}}
	ev := func(name, uid, msg string, at time.Time, useEventTime bool) *corev1.Event {
		e := &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-0", UID: "x"},
			Reason:         "FailedScheduling",
			Message:        msg,
		}
		e.InvolvedObject.UID = typesUID(uid)
		if useEventTime {
			e.EventTime = metav1.NewMicroTime(at)
		} else {
			e.LastTimestamp = metav1.NewTime(at)
		}
		return e
	}
	now := time.Now()
	client := fake.NewSimpleClientset(
		ev("old-pod", "old-uid", "stale message from the previous pod", now, false),
		ev("e1", "new-uid", "0/3 nodes are available: older", now.Add(-time.Minute), true),
		ev("e2", "new-uid", "0/3 nodes are available: newest", now.Add(-time.Second), true),
	)
	got, _, err := latestSchedulerEvent(context.Background(), client, p)
	if err != nil {
		t.Fatal(err)
	}
	if got != "0/3 nodes are available: newest" {
		t.Fatalf("got %q", got)
	}
}

// Audit risk 12: a PVC on a WaitForFirstConsumer class (including the cluster
// default class) is "waiting", an Immediate one is "unbound", and a not-yet-
// created generic ephemeral PVC is not reported as missing.
func TestUnboundPVCs_BindingModes(t *testing.T) {
	wffc, imm := storagev1.VolumeBindingWaitForFirstConsumer, storagev1.VolumeBindingImmediate
	sc := func(name string, mode storagev1.VolumeBindingMode, def bool) *storagev1.StorageClass {
		s := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, VolumeBindingMode: &mode}
		if def {
			s.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
		}
		return s
	}
	pvc := func(name string, class *string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: class},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		}
	}
	str := func(s string) *string { return &s }
	vol := func(name, claim string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}
	}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{
			vol("a", "local-data"),   // explicit WFFC class
			vol("b", "default-data"), // no class -> default class (WFFC)
			vol("c", "nfs-data"),     // Immediate class -> real blocker
			vol("d", "gone"),         // missing
			{Name: "scratch", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}},
		}},
	}
	client := fake.NewSimpleClientset(
		sc("local-path", wffc, true), sc("nfs", imm, false),
		pvc("local-data", str("local-path")), pvc("default-data", nil), pvc("nfs-data", str("nfs")),
	)
	missing, unbound, waiting, _, _ := unboundPVCs(context.Background(), client, p, map[string]storagev1.VolumeBindingMode{})
	if len(missing) != 1 || missing[0] != "gone" {
		t.Errorf("missing = %v, want [gone] (ephemeral PVC not yet created is not missing)", missing)
	}
	if len(unbound) != 1 || unbound[0] != "nfs-data" {
		t.Errorf("unbound = %v, want [nfs-data]", unbound)
	}
	if len(waiting) != 2 {
		t.Errorf("waiting = %v, want [local-data default-data]", waiting)
	}
}

// Audit risk 20: in namespace mode a Pending pod already bound to a node is
// still reported (never "No Pending pods found").
func TestPendingPods_KeepsBoundPendingPods(t *testing.T) {
	bound := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pulling", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "worker-1"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	client := fake.NewSimpleClientset(bound)
	for _, o := range []options{{namespace: "default"}, {namespace: "default", podName: "pulling"}, {allNamespaces: true}} {
		got, err := pendingPods(context.Background(), client, o)
		if err != nil || len(got) != 1 {
			t.Errorf("opts %+v: got %d pods, err %v", o, len(got), err)
		}
	}
}

func typesUID(s string) types.UID { return types.UID(s) }

// Retest #1: StorageClass unreadable (RBAC) -> "unknown", never "unbound".
func TestUnboundPVCs_StorageClassForbidden(t *testing.T) {
	str := func(s string) *string { return &s }
	client := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: str("local-path")},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	})
	client.PrependReactor("get", "storageclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, "local-path", errors.New("no RBAC"))
	})
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{
		{Name: "d", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}},
	}}}
	_, unbound, waiting, unknown, _ := unboundPVCs(context.Background(), client, p, map[string]storagev1.VolumeBindingMode{})
	if len(unbound) != 0 || len(waiting) != 0 || len(unknown) != 1 || !strings.Contains(unknown[0], "forbidden") {
		t.Fatalf("unbound=%v waiting=%v unknown=%v", unbound, waiting, unknown)
	}
}

// Review 3 B9: with several default StorageClasses the newest one wins.
func TestBindingMode_NewestDefaultWins(t *testing.T) {
	wffc, imm := storagev1.VolumeBindingWaitForFirstConsumer, storagev1.VolumeBindingImmediate
	def := map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	client := fake.NewSimpleClientset(
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "old", Annotations: def, CreationTimestamp: old}, VolumeBindingMode: &imm},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "new", Annotations: def, CreationTimestamp: metav1.Now()}, VolumeBindingMode: &wffc},
	)
	mode, err := bindingMode(context.Background(), client, &corev1.PersistentVolumeClaim{}, map[string]storagev1.VolumeBindingMode{})
	if err != nil || mode != wffc {
		t.Fatalf("mode=%s err=%v, want the newest default (WFFC)", mode, err)
	}
}

// Review 3 B6: events from the pod's own (custom) scheduler count as "seen".
func TestLatestSchedulerEvent_SeenByCustomScheduler(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"}, Spec: corev1.PodSpec{SchedulerName: "volcano"}}
	client := fake.NewSimpleClientset(&corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "x", Namespace: "default"},
		Reason:         "Unschedulable", Source: corev1.EventSource{Component: "volcano"},
	})
	_, seen, err := latestSchedulerEvent(context.Background(), client, p)
	if err != nil || !seen {
		t.Fatalf("seen=%v err=%v", seen, err)
	}
}
