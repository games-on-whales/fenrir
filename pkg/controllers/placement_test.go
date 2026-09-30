package controllers

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseTolerations(t *testing.T) {
	got, err := ParseTolerations("nvidia.com/gpu=present:NoSchedule, dedicated:NoExecute")
	if err != nil {
		t.Fatal(err)
	}
	want := []corev1.Toleration{
		{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpEqual, Value: "present", Effect: corev1.TaintEffectNoSchedule},
		{Key: "dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got, err := ParseTolerations(""); err != nil || got != nil {
		t.Fatalf("empty: %+v, %v", got, err)
	}
	for _, bad := range []string{
		"nvidia.com/gpu", "nvidia.com/gpu=present:Sometimes", ":NoSchedule", "a:NoSchedule,",
		"a :NoSchedule", "a = b:NoSchedule", "bad key!:NoSchedule", "a=b=c:NoSchedule", "=v:NoSchedule",
	} {
		if _, err := ParseTolerations(bad); err == nil {
			t.Errorf("ParseTolerations(%q) succeeded", bad)
		}
	}
}
