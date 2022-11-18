package v1

import (
	"context"
	"encoding/json"
	"fmt"
	v1alpha12 "github.com/chamhaw/kubernetes-rollout/api/v1alpha1"
	"github.com/chamhaw/kubernetes-rollout/rollout/utils/rollout"
	"github.com/chamhaw/kubernetes-rollout/rollout/v1/validation"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"reflect"
	"runtime/debug"
	"time"

	log "github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/utils/pointer"

	"github.com/chamhaw/kubernetes-rollout/rollout/utils/conditions"
	"github.com/chamhaw/kubernetes-rollout/rollout/utils/defaults"
	logutil "github.com/chamhaw/kubernetes-rollout/rollout/utils/log"
	replicasetutil "github.com/chamhaw/kubernetes-rollout/rollout/utils/replicaset"
	timeutil "github.com/chamhaw/kubernetes-rollout/rollout/utils/time"
)

// Controller is the controller implementation for Rollout resources
type Controller struct {
	reconcilerBase

	// namespace which namespace(s) operates on
	namespace string
	// rsControl is used for adopting/releasing replica sets.
	replicaSetControl controller.RSControlInterface

	rolloutWorkqueue workqueue.RateLimitingInterface
}

// ControllerConfig describes the data required to instantiate a new rollout controller
type ControllerConfig struct {
	KubeClientSet    kubernetes.Interface
	RolloutManager   v1alpha12.RolloutInterface
	RolloutWorkqueue workqueue.RateLimitingInterface
}

// reconcilerBase 是一个持有了各种 client 和基础依赖的 context
type reconcilerBase struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface

	rolloutManager v1alpha12.RolloutInterface

	// used for unit testing
	enqueueRollout func(obj interface{}) //nolint:structcheck
	// 可以用来搞延时队列
	enqueueRolloutAfter func(obj interface{}, duration time.Duration) //nolint:structcheck

	// 可以用来搞定时轮询
	resyncPeriod time.Duration
}

// NewController returns a new rollout controller
func NewController(cfg ControllerConfig) *Controller {

	replicaSetControl := controller.RealRSControl{
		KubeClient: cfg.KubeClientSet,
	}

	base := reconcilerBase{
		kubeclientset: cfg.KubeClientSet,

		rolloutManager: cfg.RolloutManager,
	}

	rolloutController := &Controller{
		reconcilerBase:    base,
		replicaSetControl: replicaSetControl,
		rolloutWorkqueue:  cfg.RolloutWorkqueue,
	}
	return rolloutController
}

func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	for i := 0; i < threadiness; i++ {
		go wait.Until(func() {
			RunWorker(c.rolloutWorkqueue, "Rollout", c.syncHandler)
		}, 5*time.Minute, stopCh)
	}
	log.Info("Started Rollout workers")

	<-stopCh
	log.Info("Shutting down workers")

	return nil
}

func (c *Controller) syncHandler(key string) error {
	startTime := timeutil.Now()
	cluster, namespace, name, err := rollout.SplitClusterNamespaceKey(key)
	if err != nil {
		return err
	}
	ctx := context.TODO()
	ro, err := c.rolloutManager.Get(ctx, cluster, namespace, name)
	if err != nil {
		return err
	}

	r := remarshalRollout(ro)
	logCtx := logutil.WithRollout(r)
	logCtx = logutil.WithVersionFields(logCtx, r)
	logCtx.Info("Started syncing rollout")

	if r.ObjectMeta.DeletionTimestamp != nil {
		logCtx.Info("No reconciliation as rollout marked for deletion")
		return nil
	}

	defer func() {
		duration := time.Since(startTime)
		logCtx.WithField("time_ms", duration.Seconds()*1e3).Info("Reconciliation completed")
	}()

	roCtx, err := c.newRolloutContext(r)
	if err != nil {
		logCtx.Errorf("newRolloutContext err %v", err)
		return err
	}

	if ro.Spec.Replicas == nil {
		logCtx.Info("Defaulting .spec.replica to 1")
		r.Spec.Replicas = pointer.Int32Ptr(defaults.DefaultReplicas)
		_, err := c.rolloutManager.Update(ctx, cluster, r)
		return err
	}

	err = roCtx.reconcile()
	// TODO 是否 store
	if err != nil {
		logCtx.Errorf("roCtx.reconcile err %v", err)
	}
	return err
}

func (c *Controller) newRolloutContext(rollout *v1alpha12.Rollout) (*rolloutContext, error) {
	// 找到当前 属于 Rollout 的 所有RS，注意，假定 selector 不会变， 如果变了可能会捞不出来孤儿RS
	rsList, err := c.getReplicaSetsForRollout(rollout)
	if err != nil {
		return nil, err
	}

	// 用pod template hash找出新的 RS
	newRS := replicasetutil.FindNewReplicaSet(rollout, rsList)
	// 找到所有老的，不是新的都是老的，包括 stable 的
	olderRSs := replicasetutil.FindOldReplicaSets(rollout, rsList, newRS)
	// stable 的 RS， 从 rollout 中的 stableRS 获取，然后在老 RS 里做过滤
	// stableRS 可能没有
	stableRS := replicasetutil.GetStableRS(rollout, newRS, olderRSs)

	// all -  new - stable
	otherRSs := replicasetutil.GetOtherRSs(rollout, newRS, stableRS, rsList)

	logCtx := logutil.WithRollout(rollout)
	roCtx := rolloutContext{
		rollout:  rollout,
		log:      logCtx,
		newRS:    newRS,
		stableRS: stableRS,
		olderRSs: olderRSs,
		otherRSs: otherRSs,
		allRSs:   rsList,
		//newStatus: v1alpha12.RolloutStatus{
		//	RestartedAt: rollout.Status.RestartedAt,
		//},
		pauseContext: &pauseContext{
			rollout: rollout,
			log:     logCtx,
		},
		reconcilerBase: c.reconcilerBase,
	}
	return &roCtx, nil
}

func (c *rolloutContext) getRolloutValidationErrors() error {
	rolloutValidationErrors := validation.ValidateRollout(c.rollout)
	if len(rolloutValidationErrors) > 0 {
		return rolloutValidationErrors[0]
	}
	return nil
}

func (c *rolloutContext) createInvalidRolloutCondition(validationError error, r *v1alpha12.Rollout) error {
	prevCond := conditions.GetRolloutCondition(r.Status, v1alpha12.InvalidSpec)
	invalidSpecCond := prevCond
	errorMessage := fmt.Sprintf("The Rollout \"%s\" is invalid: %s", r.Name, validationError.Error())
	if prevCond == nil || prevCond.Message != errorMessage {
		invalidSpecCond = conditions.NewRolloutCondition(v1alpha12.InvalidSpec, corev1.ConditionTrue, conditions.InvalidSpecReason, errorMessage)
	}
	c.log.Error(errorMessage)
	if r.Status.ObservedGeneration != r.Generation || !reflect.DeepEqual(invalidSpecCond, prevCond) {
		// SetRolloutCondition only updates the condition when the status and/or reason changes, but
		// the controller should update the invalidSpec if there is a change in why the spec is invalid
		if prevCond != nil && prevCond.Message != invalidSpecCond.Message {
			conditions.RemoveRolloutCondition(&r.Status, v1alpha12.InvalidSpec)
		}
		err := c.UpdateRolloutStatus(r, invalidSpecCond)
		if err != nil {
			return err
		}
	}
	return nil
}

func remarshalRollout(r *v1alpha12.Rollout) *v1alpha12.Rollout {
	rolloutBytes, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	var remarshalled v1alpha12.Rollout
	err = json.Unmarshal(rolloutBytes, &remarshalled)
	if err != nil {
		panic(err)
	}
	return &remarshalled
}

// RunWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func RunWorker(workqueue workqueue.RateLimitingInterface, objType string, syncHandler func(string) error) {
	for processNextWorkItem(workqueue, objType, syncHandler) {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func processNextWorkItem(workqueue workqueue.RateLimitingInterface, objType string, syncHandler func(string) error) bool {
	obj, shutdown := workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer workqueue.Done(obj)
		var key string
		var ok bool
		// cluster/namespace/name
		if key, ok = obj.(string); !ok {
			workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		logCtx := log.WithField(objType, key)
		runSyncHandler := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					logCtx.Errorf("Key[%s] Recovered from panic: %+v\n%s", key, r, debug.Stack())
					err = fmt.Errorf("recovered from Panic")
				}
			}()
			return syncHandler(key)
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Rollout resource to be synced.
		if err := runSyncHandler(); err != nil {
			logCtx.Errorf("%s syncHandler error: %v", objType, err)
			// Put the item back on
			// the workqueue to handle any transient errors.
			workqueue.AddRateLimited(key)

			logCtx.Infof("%s syncHandler queue retries: %v : key \"%v\"", objType, workqueue.NumRequeues(key), key)
			return err
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		workqueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}
