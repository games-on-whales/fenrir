package controllers

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ParseTolerations parses a comma-separated list of taints to tolerate, each
// "key[=value]:Effect" as kubectl taint prints them, e.g.
// "nvidia.com/gpu=present:NoSchedule". A key without a value tolerates any
// value. Session pods are pinned to one node, and that node may be tainted
// (talos04 is, as a GPU node), so its taints must be tolerable from config.
func ParseTolerations(s string) ([]corev1.Toleration, error) {
	var tolerations []corev1.Toleration
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	for item := range strings.SplitSeq(s, ",") {
		keyValue, effect, ok := strings.Cut(strings.TrimSpace(item), ":")
		if !ok || keyValue == "" {
			return nil, fmt.Errorf("toleration %q: want key[=value]:Effect", item)
		}
		t := corev1.Toleration{Effect: corev1.TaintEffect(effect), Operator: corev1.TolerationOpExists}
		switch t.Effect {
		case corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
		default:
			return nil, fmt.Errorf("toleration %q: effect must be NoSchedule, PreferNoSchedule or NoExecute", item)
		}
		t.Key = keyValue
		if key, value, hasValue := strings.Cut(keyValue, "="); hasValue {
			t.Key, t.Value, t.Operator = key, value, corev1.TolerationOpEqual
		}
		// Validate here as the apiserver would: a typo must stop the operator
		// at startup, not fail every session Deployment apply later.
		errs := validation.IsQualifiedName(t.Key)
		errs = append(errs, validation.IsValidLabelValue(t.Value)...)
		if len(errs) > 0 {
			return nil, fmt.Errorf("toleration %q: %s", item, strings.Join(errs, "; "))
		}
		tolerations = append(tolerations, t)
	}
	return tolerations, nil
}
