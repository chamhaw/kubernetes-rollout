package v1alpha1

import (
	"k8s.io/apimachinery/pkg/util/intstr"
)

type CanaryStrategy struct {
	Steps          []CanaryStep        `json:"steps,omitempty" protobuf:"bytes,3,rep,name=steps"`
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty" protobuf:"bytes,5,opt,name=maxUnavailable"`
	MaxSurge       *intstr.IntOrString `json:"maxSurge,omitempty" protobuf:"bytes,6,opt,name=maxSurge"`
	// CanaryMetadata specify labels and annotations which will be attached to the canary pods for
	// the duration which they act as a canary, and will be removed after
	CanaryMetadata *PodTemplateMetadata `json:"canaryMetadata,omitempty" protobuf:"bytes,9,opt,name=canaryMetadata"`
	// StableMetadata specify labels and annotations which will be attached to the stable pods for
	// the duration which they act as a canary, and will be removed after
	StableMetadata *PodTemplateMetadata `json:"stableMetadata,omitempty" protobuf:"bytes,10,opt,name=stableMetadata"`
}

// CanaryStep defines a step of a canary deployment.
type CanaryStep struct {
	// SetWeight 流量 + 副本数， 都需要根据这个做出调整，我们目前暂不支持流量，只支持副本数，所以目前和 SetCanaryScale.Weight 作用一样
	SetWeight *int32 `json:"setWeight,omitempty" protobuf:"varint,1,opt,name=setWeight"`
	// Pause freezes the rollout by setting spec.Paused to true.
	// A Rollout will resume when spec.Paused is reset to false.
	// +optional
	Pause *RolloutPause `json:"pause,omitempty" protobuf:"bytes,2,opt,name=pause"`
	// SetCanaryScale defines how to scale the newRS without changing traffic weight
	// +optional
	SetCanaryScale *SetCanaryScale `json:"setCanaryScale,omitempty" protobuf:"bytes,5,opt,name=setCanaryScale"`
}

// SetCanaryScale defines how to scale the newRS without changing traffic weight
type SetCanaryScale struct {
	// Weight sets the percentage of replicas the newRS should have
	// +optional
	Weight *int32 `json:"weight,omitempty" protobuf:"varint,1,opt,name=weight"`
	// Replicas sets the number of replicas the newRS should have
	// +optional
	Replicas *int32 `json:"replicas,omitempty" protobuf:"varint,2,opt,name=replicas"`
}

type CanaryStatus struct{}
