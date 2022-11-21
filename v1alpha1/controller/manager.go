package controller

import (
	"context"
	apiv1alpha1 "github.com/chamhaw/kubernetes-rollout-api/v1alpha1"
	"github.com/chamhaw/kubernetes-rollout/v1alpha1"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"
)

// Manager is the controller implementation for Argo-Rollout resources
type Manager struct {
	rolloutController *v1.Controller

	rolloutWorkQueue workqueue.Interface

	kubeClientSet kubernetes.Interface
}

// NewManager returns a new manager to manage all the controllers
func NewManager(kubeclientset kubernetes.Interface, rolloutManager apiv1alpha1.RolloutInterface, rolloutWorkqueue workqueue.RateLimitingInterface) *Manager {

	rolloutController := v1.NewController(v1.ControllerConfig{
		KubeClientSet:    kubeclientset,
		RolloutManager:   rolloutManager,
		RolloutWorkqueue: rolloutWorkqueue,
	})

	cm := &Manager{
		//replicasSetSynced: replicaSetInformer.Informer().HasSynced,
		rolloutController: rolloutController,
		rolloutWorkQueue:  rolloutWorkqueue,
		kubeClientSet:     kubeclientset,
	}
	return cm
}

func (c *Manager) StartLeading(ctx context.Context) {
	defer runtime.HandleCrash()
	go c.rolloutController.Run(1, ctx.Done())
	log.Infof("Started Rollout")
}
