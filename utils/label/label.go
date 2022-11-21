package label

import (
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func ExtractLabelSelector(v map[string]interface{}) (*metav1.LabelSelector, error) {
	labels, _, _ := unstructured.NestedStringMap(v, "spec", "selector", "matchLabels")
	items, _, _ := unstructured.NestedSlice(v, "spec", "selector", "matchExpressions")
	var matchExpressions []metav1.LabelSelectorRequirement
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("unable to retrieve matchExpressions for object, item %v is not a map", item)
		}
		out := metav1.LabelSelectorRequirement{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &out); err != nil {
			return nil, fmt.Errorf("unable to retrieve matchExpressions for object: %v", err)
		}
		matchExpressions = append(matchExpressions, out)
	}
	return &metav1.LabelSelector{
		MatchLabels:      labels,
		MatchExpressions: matchExpressions,
	}, nil
}
