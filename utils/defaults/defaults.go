package defaults

import (
	"github.com/chamhaw/kubernetes-rollout-api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// DefaultReplicas default number of replicas for a rollout if the .Spec.Replicas is nil
	DefaultReplicas = int32(1)
	// DefaultRevisionHistoryLimit default number of revisions to keep if .Spec.RevisionHistoryLimit is nil
	DefaultRevisionHistoryLimit = int32(10)
	// DefaultMaxSurge default number for the max number of additional pods that can be brought up during a rollout
	DefaultMaxSurge = "1"
	// DefaultMaxUnavailable default number for the max number of unavailable pods during a rollout
	DefaultMaxUnavailable = "0"
	// DefaultProgressDeadlineSeconds default number of seconds for the rollout to be making progress
	DefaultProgressDeadlineSeconds = int32(600)
)

func GetStringOrDefault(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	} else {
		return value
	}
}

// GetReplicasOrDefault returns the deferenced number of replicas or the default number
func GetReplicasOrDefault(replicas *int32) int32 {
	if replicas == nil {
		return DefaultReplicas
	}
	return *replicas
}

// GetRevisionHistoryLimitOrDefault returns the specified number of replicas in a rollout or the default number
func GetRevisionHistoryLimitOrDefault(rollout *v1alpha1.Rollout) int32 {
	if rollout.Spec.RevisionHistoryLimit == nil {
		return DefaultRevisionHistoryLimit
	}
	return *rollout.Spec.RevisionHistoryLimit
}

func GetMaxSurgeOrDefault(rollout *v1alpha1.Rollout) *intstr.IntOrString {
	if rollout.Spec.Strategy.Canary != nil && rollout.Spec.Strategy.Canary.MaxSurge != nil {
		return rollout.Spec.Strategy.Canary.MaxSurge
	}
	defaultValue := intstr.FromString(DefaultMaxSurge)
	return &defaultValue
}

func GetMaxUnavailableOrDefault(rollout *v1alpha1.Rollout) *intstr.IntOrString {
	if rollout.Spec.Strategy.Canary != nil && rollout.Spec.Strategy.Canary.MaxUnavailable != nil {
		return rollout.Spec.Strategy.Canary.MaxUnavailable
	}
	defaultValue := intstr.FromString(DefaultMaxUnavailable)
	return &defaultValue
}

func GetProgressDeadlineSecondsOrDefault(rollout *v1alpha1.Rollout) int32 {
	return DefaultProgressDeadlineSeconds
}
