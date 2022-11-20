package log

import (
	"flag"
	"github.com/chamhaw/kubernetes-rollout-api/v1alpha1"
	log "github.com/sirupsen/logrus"
	"k8s.io/klog/v2"
	"strconv"
)

const (
	// RolloutKey defines the key for the rollout field
	RolloutKey = "rollout"
	// NamespaceKey defines the key for the namespace field
	NamespaceKey = "namespace"

	ClusterKey = "cluster"
)

// SetKLogLevel set the klog level for the k8s go-client
func SetKLogLevel(klogLevel int) {
	klog.InitFlags(nil)
	_ = flag.Set("logtostderr", "true")
	_ = flag.Set("v", strconv.Itoa(klogLevel))
}

// WithRollout returns a logging context for Rollouts
func WithRollout(rollout *v1alpha1.Rollout) *log.Entry {
	return log.WithField(RolloutKey, rollout.Name).WithField(NamespaceKey, rollout.Namespace)
}

func WithVersionFields(entry *log.Entry, r *v1alpha1.Rollout) *log.Entry {
	return entry.WithFields(map[string]interface{}{
		"resourceVersion": r.ResourceVersion,
		"generation":      r.Generation,
	})
}
