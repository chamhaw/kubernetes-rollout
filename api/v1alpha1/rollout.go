package v1alpha1

import (
	"context"
)

// RolloutsGetter has a method to return a RolloutInterface.
// A group's client should implement this interface.
type RolloutsGetter interface {
	Rollouts(namespace string) RolloutInterface
}

// RolloutInterface has methods to work with Rollout resources.
type RolloutInterface interface {
	Create(ctx context.Context, clusterCode string, rollout *Rollout) (*Rollout, error)
	Update(ctx context.Context, clusterCode string, rollout *Rollout) (*Rollout, error)
	UpdateStatus(ctx context.Context, clusterCode string, rollout *Rollout) (*Rollout, error)
	Delete(ctx context.Context, clusterCode, namespace, name string) error
	Get(ctx context.Context, clusterCode, namespace, name string) (*Rollout, error)
}
