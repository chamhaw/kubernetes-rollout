package v1

import (
	"context"
	"fmt"
	"github.com/chamhaw/kubernetes-rollout/api/v1alpha1"
	"github.com/chamhaw/kubernetes-rollout/rollout/utils/annotations"
	"github.com/chamhaw/kubernetes-rollout/rollout/utils/conditions"
	"github.com/chamhaw/kubernetes-rollout/rollout/utils/defaults"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/controller"
	labelsutil "k8s.io/kubernetes/pkg/util/labels"
	"k8s.io/utils/pointer"
	"sort"
	"strconv"

	"github.com/chamhaw/kubernetes-rollout/rollout/utils/hash"
	logutil "github.com/chamhaw/kubernetes-rollout/rollout/utils/log"
	replicasetutil "github.com/chamhaw/kubernetes-rollout/rollout/utils/replicaset"
	rolloututil "github.com/chamhaw/kubernetes-rollout/rollout/utils/rollout"
)

// 拿到 新的RS， 并更新掉 revision。 注意，新的RS不存在则创建
func (c *rolloutContext) getNewReplicaSetAndSyncRevision(createIfNotExisted bool) (*appsv1.ReplicaSet, error) {
	// 获取新 RS，如果有，就返回， 否则创建
	newRS, err := c.syncReplicaSetRevision()
	if err != nil {
		return nil, err
	}
	if newRS == nil && createIfNotExisted {
		newRS, err = c.createDesiredReplicaSet()
		if err != nil {
			return nil, err
		}
	}
	return newRS, nil
}

// 从context中拿到新RS， 如果revision不是最新，则更新到最大+ 1，以及更新了一些其他的annotation
// 更新了亲和配置以及rollout状态
func (c *rolloutContext) syncReplicaSetRevision() (*appsv1.ReplicaSet, error) {
	if c.newRS == nil {
		return nil, nil
	}
	ctx := context.TODO()

	// old里面的最大revision
	maxOldRevision := replicasetutil.MaxRevision(c.olderRSs)
	// 新 revision 是 最大 + 1
	newRevision := strconv.FormatInt(maxOldRevision+1, 10)

	// 深拷贝一份，防止并发
	rsCopy := c.newRS.DeepCopy()

	// Set existing new replica set's annotation
	annotationsUpdated := annotations.SetNewReplicaSetAnnotations(c.rollout, rsCopy, newRevision, true)
	minReadySecondsNeedsUpdate := rsCopy.Spec.MinReadySeconds != c.rollout.Spec.MinReadySeconds
	affinityNeedsUpdate := replicasetutil.IfInjectedAntiAffinityRuleNeedsUpdate(rsCopy.Spec.Template.Spec.Affinity, *c.rollout)

	// 这仨任意一个有更新，则触发update
	if annotationsUpdated || minReadySecondsNeedsUpdate || affinityNeedsUpdate {
		rsCopy.Spec.MinReadySeconds = c.rollout.Spec.MinReadySeconds
		rsCopy.Spec.Template.Spec.Affinity = replicasetutil.GenerateReplicaSetAffinity(*c.rollout)
		return c.kubeclientset.AppsV1().ReplicaSets(rsCopy.ObjectMeta.Namespace).Update(ctx, rsCopy, metav1.UpdateOptions{})
	}

	// 执行 Rollout 的 revision更新
	if err := c.setRolloutRevision(rsCopy.Annotations[annotations.RevisionAnnotation]); err != nil {
		return nil, err
	}

	// If no other Progressing condition has been recorded and we need to estimate the progress
	// of this rollout then it is likely that old users started caring about progress. In that
	// case we need to take into account the first time we noticed their new replica set.
	cond := conditions.GetRolloutCondition(c.rollout.Status, v1alpha1.RolloutProgressing)
	if cond == nil {
		msg := fmt.Sprintf(conditions.FoundNewRSMessage, rsCopy.Name)
		condition := conditions.NewRolloutCondition(v1alpha1.RolloutProgressing, corev1.ConditionTrue, conditions.FoundNewRSReason, msg)
		conditions.SetRolloutCondition(&c.rollout.Status, *condition)
		updatedRollout, err := c.rolloutManager.UpdateStatus(ctx, c.rollout.Annotations[annotations.ClusterCodeAnnotation], c.rollout)
		if err != nil {
			c.log.WithError(err).Error("Error: updating rollout revision")
			return nil, err
		}
		c.rollout = updatedRollout
		c.log.Infof("Initialized Progressing condition: %v", condition)
	}
	return rsCopy, nil
}

func (c *rolloutContext) setRolloutRevision(revision string) error {
	if annotations.SetRolloutRevision(c.rollout, revision) {
		updatedRollout, err := c.rolloutManager.Update(context.TODO(), c.rollout.Annotations[annotations.ClusterCodeAnnotation], c.rollout)
		if err != nil {
			c.log.WithError(err).Error("Error: updating rollout revision")
			return err
		}
		c.rollout = updatedRollout.DeepCopy()
	}
	return nil
}

// createDesiredReplicaSet 创建新RS
func (c *rolloutContext) createDesiredReplicaSet() (*appsv1.ReplicaSet, error) {
	ctx := context.TODO()
	// 找到 old里面的最大
	maxOldRevision := replicasetutil.MaxRevision(c.olderRSs)
	// 新的 revision = 最大 + 1
	newRevision := strconv.FormatInt(maxOldRevision+1, 10)

	// 准备 pod template
	newRSTemplate := *c.rollout.Spec.Template.DeepCopy()
	// 默认的亲和和反亲和策略
	newRSTemplate.Spec.Affinity = replicasetutil.GenerateReplicaSetAffinity(*c.rollout)
	// 算 pod template hash
	podTemplateSpecHash := hash.ComputePodTemplateHash(&c.rollout.Spec.Template, c.rollout.Status.CollisionCount)
	// 把算出来的 hash 加到新pod的label里。
	newRSTemplate.Labels = labelsutil.CloneAndAddLabel(c.rollout.Spec.Template.Labels, v1alpha1.DefaultRolloutUniqueLabelKey, podTemplateSpecHash)
	// 把算出来的 hash 加到新RS的label里。
	newRSSelector := labelsutil.CloneSelectorAndAddLabel(c.rollout.Spec.Selector, v1alpha1.DefaultRolloutUniqueLabelKey, podTemplateSpecHash)

	// 创建新RS，填充RS基本属性
	newRS := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			// 新RS名字是 rollout名字组合了 pod template 的 hash
			Name:            c.rollout.Name + "-" + podTemplateSpecHash,
			Namespace:       c.rollout.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(c.rollout, c.rollout.GroupVersionKind())},
			Labels:          newRSTemplate.Labels,
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas:        new(int32),
			MinReadySeconds: c.rollout.Spec.MinReadySeconds,
			Selector:        newRSSelector,
			Template:        newRSTemplate,
		},
	}
	newRS.Spec.Replicas = pointer.Int32(0)
	// 给新RD填充一些annotation信息
	annotations.SetNewReplicaSetAnnotations(c.rollout, newRS, newRevision, false)

	if c.rollout.Spec.Strategy.Canary != nil {
		var ephemeralMetadata *v1alpha1.PodTemplateMetadata
		if c.stableRS != nil && c.stableRS != c.newRS {
			// If this is a canary rollout, with ephemeral *canary* metadata, and there is a stable RS,
			// then inject the canary metadata so that all the RS's new pods get the canary labels/annotation
			if c.rollout.Spec.Strategy.Canary != nil {
				ephemeralMetadata = c.rollout.Spec.Strategy.Canary.CanaryMetadata
			}
		} else {
			// Otherwise, if stableRS is nil, we are in a brand-new rollout and then this replicaset
			// will eventually become the stableRS, so we should inject the stable labels/annotation
			if c.rollout.Spec.Strategy.Canary != nil {
				ephemeralMetadata = c.rollout.Spec.Strategy.Canary.StableMetadata
			}
		}
		newRS, _ = replicasetutil.SyncReplicaSetEphemeralPodMetadata(newRS, ephemeralMetadata)
	}

	alreadyExists := false
	createdRS, err := c.kubeclientset.AppsV1().ReplicaSets(c.rollout.Namespace).Create(ctx, newRS, metav1.CreateOptions{})
	switch {
	// 如果遇到409， 需要比对 hash
	case errors.IsAlreadyExists(err):
		alreadyExists = true

		//  拿到已经存在的RS
		rs, rsErr := c.kubeclientset.AppsV1().ReplicaSets(newRS.Namespace).Get(ctx, newRS.Name, metav1.GetOptions{})
		if rsErr != nil {
			return nil, rsErr
		}

		// 如果递归比较 PodTemplateSpec 等相等，则认为当前RS就是新的，不需要再创建了
		controllerRef := metav1.GetControllerOf(rs)
		if controllerRef != nil && controllerRef.UID == c.rollout.UID && replicasetutil.PodTemplateEqualIgnoreHash(&rs.Spec.Template, &c.rollout.Spec.Template) {
			createdRS = rs
			err = nil
			break
		}

		// 增加碰撞次数，更新到 status里，重新 走流程
		if c.rollout.Status.CollisionCount == nil {
			c.rollout.Status.CollisionCount = new(int32)
		}
		preCollisionCount := *c.rollout.Status.CollisionCount
		// 暂不维护盖子段
		*c.rollout.Status.CollisionCount++
		_, roErr := c.rolloutManager.UpdateStatus(ctx, c.rollout.Annotations[annotations.ClusterCodeAnnotation], c.rollout)
		if roErr == nil {
			c.log.Warnf("Found a hash collision - bumped collisionCount (%d->%d) to resolve it", preCollisionCount, *c.rollout.Status.CollisionCount)
		}
		return nil, err
		// 其他错误的情况， 记录错误，并返回
	case err != nil:
		msg := fmt.Sprintf(conditions.FailedRSCreateMessage, newRS.Name, err)
		cond := conditions.NewRolloutCondition(v1alpha1.RolloutProgressing, corev1.ConditionFalse, conditions.FailedRSCreateReason, msg)
		patchErr := c.UpdateRolloutStatus(c.rollout, cond)
		if patchErr != nil {
			c.log.Warnf("Error Patching Rollout: %s", patchErr.Error())
		}
		return nil, err
	default:
		c.log.Infof("Created ReplicaSet %s", createdRS.Name)
	}

	// 不管的成功创建了新RS还是找到了新RS，都要更新 revision
	if err := c.setRolloutRevision(newRevision); err != nil {
		return nil, err
	}

	if alreadyExists {
		return createdRS, err
	}

	// 以下操作是只有再本流程里真正完成了创建新RS操作时进行
	// 更新 condition
	msg := fmt.Sprintf(conditions.NewReplicaSetMessage, createdRS.Name)
	condition := conditions.NewRolloutCondition(v1alpha1.RolloutProgressing, corev1.ConditionTrue, conditions.NewReplicaSetReason, msg)
	conditions.SetRolloutCondition(&c.rollout.Status, *condition)
	updatedRollout, err := c.rolloutManager.UpdateStatus(ctx, c.rollout.Annotations[annotations.ClusterCodeAnnotation], c.rollout)
	if err != nil {
		return nil, err
	}
	c.rollout = updatedRollout.DeepCopy()
	c.log.Infof("Set rollout condition: %v", condition)
	return createdRS, err
}

// syncReplicasOnly is responsible for reconciling rollouts on scaling events.
func (c *rolloutContext) syncReplicasOnly() error {
	c.log.Infof("Syncing replicas only due to scaling event")
	_, err := c.getNewReplicaSetAndSyncRevision(false)
	if err != nil {
		return err
	}
	// The controller wants to use the rolloutCanary method to reconcile the rollout if the rollout is not paused.
	// If there are no scaling events, the rollout should only sync its status
	if c.rollout.Spec.Strategy.Canary != nil {
		if _, err := c.reconcileCanaryReplicaSets(); err != nil {
			// If we get an error while trying to scale, the rollout will be requeued
			// so we can abort this resync
			return err
		}
		return c.tryToCompleteStepAndSyncRolloutStatusCanary()
	}
	return fmt.Errorf("no rollout strategy provided")
}

func (c *rolloutContext) scaleReplicaSet(rs *appsv1.ReplicaSet, newScale int32) (bool, *appsv1.ReplicaSet, error) {
	// No need to scale
	// && !annotations.ReplicasAnnotationsNeedUpdate(rs, defaults.GetReplicasOrDefault(c.rollout.Spec.Replicas))
	if *(rs.Spec.Replicas) == newScale {
		return false, rs, nil
	}
	c.log.Infof("Scale RS[%s] from %d to %d", rs.Name, *rs.Spec.Replicas, newScale)
	// 伸缩 pod
	scaled, newRS, err := c.doScaleReplicaSet(rs, newScale, c.rollout)
	return scaled, newRS, err
}

// scaleReplicaSet 伸缩 RS 操作
func (c *rolloutContext) doScaleReplicaSet(rs *appsv1.ReplicaSet, newScale int32, rollout *v1alpha1.Rollout) (bool, *appsv1.ReplicaSet, error) {
	ctx := context.TODO()
	// 前置检查，如果预期副本数和信副本数一致，则不需要更新
	sizeNeedsUpdate := *(rs.Spec.Replicas) != newScale
	// 如果信副本数是 0，则代表全部 pod 杀掉
	fullScaleDown := newScale == int32(0)
	// 获取 rollout 的副本数
	rolloutReplicas := defaults.GetReplicasOrDefault(rollout.Spec.Replicas)

	scaled := false
	var err error
	if sizeNeedsUpdate {
		rsCopy := rs.DeepCopy()
		*(rsCopy.Spec.Replicas) = newScale
		annotations.SetReplicasAnnotations(rsCopy, rolloutReplicas)
		if fullScaleDown {
			delete(rsCopy.Annotations, v1alpha1.DefaultReplicaSetScaleDownDeadlineAnnotationKey)
		}
		rs, err = c.kubeclientset.AppsV1().ReplicaSets(rsCopy.Namespace).Update(ctx, rsCopy, metav1.UpdateOptions{})
		if err == nil && sizeNeedsUpdate {
			scaled = true
		}
	}
	return scaled, rs, err
}

// 维护基本状态
// calculateStatus calculates the common fields for all rollouts by looking into the provided replica sets.
func (c *rolloutContext) calculateBaseStatus() v1alpha1.RolloutStatus {
	// 保存 rollout 的状态
	prevStatus := c.rollout.Status

	//取出 InvalidSpec 的 cond
	prevCond := conditions.GetRolloutCondition(prevStatus, v1alpha1.InvalidSpec)
	err := c.getRolloutValidationErrors()
	if err == nil && prevCond != nil {
		conditions.RemoveRolloutCondition(&prevStatus, v1alpha1.InvalidSpec)
	}

	var currentPodHash string
	if c.newRS == nil {
		currentPodHash = hash.ComputePodTemplateHash(&c.rollout.Spec.Template, c.rollout.Status.CollisionCount)
		c.log.Infof("Assuming %s for new replicaset pod hash", currentPodHash)
	} else {
		currentPodHash = c.newRS.Labels[v1alpha1.DefaultRolloutUniqueLabelKey]
	}

	// 新建一个status。 每一次的更新，都是一个新的status，重新计算
	newStatus := c.newStatus
	newStatus.CurrentPodHash = currentPodHash
	newStatus.Replicas = replicasetutil.GetActualReplicaCountForReplicaSets(c.allRSs)
	newStatus.UpdatedReplicas = replicasetutil.GetActualReplicaCountForReplicaSets([]*appsv1.ReplicaSet{c.newRS})
	newStatus.ReadyReplicas = replicasetutil.GetReadyReplicaCountForReplicaSets(c.allRSs)
	newStatus.CollisionCount = c.rollout.Status.CollisionCount
	newStatus.Conditions = prevStatus.Conditions
	newStatus.ObservedGeneration = c.rollout.Generation
	c.log.Infof("Spec Generation: %d, previous ObservedGeneration: %d", c.rollout.Generation, prevStatus.ObservedGeneration)
	newStatus.PauseConditions = prevStatus.PauseConditions
	// 如果没有pause condition，则将 controller pause重置
	if !c.pauseContext.HasAddPause() {
		newStatus.ControllerPause = false
	}
	return newStatus
}

// reconcileRevisionHistoryLimit is responsible for cleaning up a rollout ie. retains all but the latest N old replica sets
// where N=r.Spec.RevisionHistoryLimit. Old replica sets are older versions of the podtemplate of a rollout kept
// around by default 1) for historical reasons.
func (c *rolloutContext) reconcileRevisionHistoryLimit(oldRSs []*appsv1.ReplicaSet) error {
	ctx := context.TODO()
	revHistoryLimit := defaults.GetRevisionHistoryLimitOrDefault(c.rollout)

	// Avoid deleting replica set with deletion timestamp set
	aliveFilter := func(rs *appsv1.ReplicaSet) bool {
		return rs != nil && rs.ObjectMeta.DeletionTimestamp == nil
	}
	cleanableRSes := controller.FilterReplicaSets(oldRSs, aliveFilter)

	diff := int32(len(cleanableRSes)) - revHistoryLimit
	if diff <= 0 {
		return nil
	}
	c.log.Infof("Cleaning up %d old replicasets from revision history limit %d", len(cleanableRSes), revHistoryLimit)

	sort.Sort(controller.ReplicaSetsByCreationTimestamp(cleanableRSes))

	c.log.Info("Looking to cleanup old replica sets")
	for i := int32(0); i < diff; i++ {
		rs := cleanableRSes[i]
		// Avoid delete replica set with non-zero replica counts
		if rs.Status.Replicas != 0 || *(rs.Spec.Replicas) != 0 || rs.Generation > rs.Status.ObservedGeneration || rs.DeletionTimestamp != nil {
			continue
		}
		c.log.Infof("Trying to cleanup replica set %q", rs.Name)
		if err := c.kubeclientset.AppsV1().ReplicaSets(rs.Namespace).Delete(ctx, rs.Name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			// Return error instead of aggregating and continuing DELETEs on the theory
			// that we may be overloading the api server.
			return err
		}
	}

	return nil
}

// checkPausedConditions checks if the given rollout is paused or not and adds an appropriate condition.
// These conditions are needed so that we won't accidentally report lack of progress for resumed rollouts
// that were paused for longer than progressDeadlineSeconds.
func (c *rolloutContext) checkPausedConditions() error {
	// Progressing condition
	progCond := conditions.GetRolloutCondition(c.rollout.Status, v1alpha1.RolloutProgressing)
	progCondPaused := progCond != nil && progCond.Reason == conditions.RolloutPausedReason

	isPaused := len(c.rollout.Status.PauseConditions) > 0 || c.rollout.Spec.Paused

	var updatedConditions []*v1alpha1.RolloutCondition

	if isPaused != progCondPaused {
		if isPaused {
			updatedConditions = append(updatedConditions, conditions.NewRolloutCondition(v1alpha1.RolloutProgressing, corev1.ConditionUnknown, conditions.RolloutPausedReason, conditions.RolloutPausedMessage))
		} else {
			updatedConditions = append(updatedConditions, conditions.NewRolloutCondition(v1alpha1.RolloutProgressing, corev1.ConditionUnknown, conditions.RolloutResumedReason, conditions.RolloutResumedMessage))
		}
	}

	pauseCond := conditions.GetRolloutCondition(c.rollout.Status, v1alpha1.RolloutPaused)
	pausedCondTrue := pauseCond != nil && pauseCond.Status == corev1.ConditionTrue

	if isPaused != pausedCondTrue {
		condStatus := corev1.ConditionFalse
		if isPaused {
			condStatus = corev1.ConditionTrue
		}
		updatedConditions = append(updatedConditions, conditions.NewRolloutCondition(v1alpha1.RolloutPaused, condStatus, conditions.RolloutPausedReason, conditions.RolloutPausedMessage))
	}

	if len(updatedConditions) == 0 {
		return nil
	}

	err := c.UpdateRolloutStatus(c.rollout, updatedConditions...)
	return err
}

// 更改condition
func (c *rolloutContext) UpdateRolloutStatus(r *v1alpha1.Rollout, conditionList ...*v1alpha1.RolloutCondition) error {
	ctx := context.TODO()
	for _, condition := range conditionList {
		conditions.SetRolloutCondition(&r.Status, *condition)
	}

	c.log.Infof("Update ObservedGeneration from %d to spec generation %d", c.rollout.Status.ObservedGeneration, c.rollout.Generation)
	r.Status.ObservedGeneration = c.rollout.Generation
	r.Status.Phase, r.Status.Message = rolloututil.CalculateRolloutPhase(r.Spec, r.Status)

	logCtx := logutil.WithVersionFields(c.log, r)

	_, err := c.rolloutManager.UpdateStatus(ctx, r.Annotations[annotations.ClusterCodeAnnotation], r)
	if err != nil {
		logCtx.Warnf("Error patching rollout: %v", err)
		return err
	}
	logCtx.Infof("Updated rollout status: %s/%s", r.Namespace, r.Name)
	return nil
}

// isIndefiniteStep returns whether or not the rollout is at an Experiment or Analysis or Pause step which should
// not affect the progressDeadlineSeconds
func isIndefiniteStep(r *v1alpha1.Rollout) bool {
	currentStep, _ := replicasetutil.GetCurrentCanaryStep(r)
	if currentStep != nil && (currentStep.Pause != nil) {
		return true
	}
	return false
}

// Rollout的 Condition 计算， 有副作用，直接再入参 status上进行判断和修改，返回的status就是入参
func (c *rolloutContext) calculateRolloutConditions(newStatus v1alpha1.RolloutStatus) v1alpha1.RolloutStatus {
	isPaused := len(c.rollout.Status.PauseConditions) > 0 || c.rollout.Spec.Paused

	var becameUnhealthy bool // remember if we transitioned from healthy to unhealthy
	completeCond := conditions.GetRolloutCondition(c.rollout.Status, v1alpha1.RolloutHealthy)
	if !isPaused && conditions.RolloutHealthy(c.rollout, &newStatus) {
		updateHealthyCond := conditions.NewRolloutCondition(v1alpha1.RolloutHealthy, corev1.ConditionTrue, conditions.RolloutHealthyReason, conditions.RolloutHealthyMessage)
		conditions.SetRolloutCondition(&newStatus, *updateHealthyCond)
	} else {
		if completeCond != nil {
			updateHealthyCond := conditions.NewRolloutCondition(v1alpha1.RolloutHealthy, corev1.ConditionFalse, conditions.RolloutHealthyReason, conditions.RolloutNotHealthyMessage)
			becameUnhealthy = conditions.SetRolloutCondition(&newStatus, *updateHealthyCond)
		}
	}

	// If there is only one replica set that is active then that means we are not running
	// a new rollout and this is a resync where we don't need to estimate any progress.
	// In such a case, we should simply not estimate any progress for this rollout.
	currentCond := conditions.GetRolloutCondition(c.rollout.Status, v1alpha1.RolloutProgressing)

	isHealthyRollout := newStatus.Replicas == newStatus.AvailableReplicas && currentCond != nil && currentCond.Reason == conditions.NewRSAvailableReason && currentCond.Type != v1alpha1.RolloutProgressing
	// Check for progress. Only do this if the latest rollout hasn't completed yet and it is not aborted
	if !isHealthyRollout {
		switch {
		case conditions.RolloutHealthy(c.rollout, &newStatus):
			// Update the rollout conditions with a message for the new replica set that
			// was successfully deployed. If the condition already exists, we ignore this update.
			rsName := ""
			if c.newRS != nil {
				rsName = c.newRS.Name
			}
			msg := fmt.Sprintf(conditions.ReplicaSetCompletedMessage, rsName)
			progressingCondition := conditions.NewRolloutCondition(v1alpha1.RolloutProgressing, corev1.ConditionTrue, conditions.NewRSAvailableReason, msg)
			conditions.SetRolloutCondition(&newStatus, *progressingCondition)
		case conditions.RolloutProgressing(c.rollout, &newStatus) || becameUnhealthy:
			// If there is any progress made, continue by not checking if the rollout failed. This
			// behavior emulates the canaryr progressDeadline check.
			msg := fmt.Sprintf(conditions.RolloutProgressingMessage, c.rollout.Name)
			if c.newRS != nil {
				msg = fmt.Sprintf(conditions.ReplicaSetProgressingMessage, c.newRS.Name)
			}

			var reason string
			if newStatus.StableRS == newStatus.CurrentPodHash && becameUnhealthy {
				// When a fully promoted rollout becomes Incomplete, e.g., due to the ReplicaSet status changes like
				// pod restarts, evicted -> recreated, we'll need to reset the rollout's condition to `PROGRESSING` to
				// avoid any timeouts.
				reason = conditions.ReplicaSetNotAvailableReason
				msg = conditions.NotAvailableMessage
			} else {
				reason = conditions.ReplicaSetUpdatedReason
			}
			condition := conditions.NewRolloutCondition(v1alpha1.RolloutProgressing, corev1.ConditionTrue, reason, msg)

			// Update the current Progressing condition or add a new one if it doesn't exist.
			// If a Progressing condition with status=true already exists, we should update
			// everything but lastTransitionTime. SetRolloutCondition already does that but
			// it also is not updating conditions when the reason of the new condition is the
			// same as the old. The Progressing condition is a special case because we want to
			// update with the same reason and change just lastUpdateTime iff we notice any
			// progress. That's why we handle it here.
			if currentCond != nil {
				if currentCond.Status == corev1.ConditionTrue {
					condition.LastTransitionTime = currentCond.LastTransitionTime
				}
				conditions.RemoveRolloutCondition(&newStatus, v1alpha1.RolloutProgressing)
			}
			conditions.SetRolloutCondition(&newStatus, *condition)
		case !isIndefiniteStep(c.rollout) && conditions.RolloutTimedOut(c.rollout, &newStatus):
			// Update the rollout with a timeout condition. If the condition already exists,
			// we ignore this update.
			msg := fmt.Sprintf(conditions.RolloutTimeOutMessage, c.rollout.Name)
			if c.newRS != nil {
				msg = fmt.Sprintf(conditions.ReplicaSetTimeOutMessage, c.newRS.Name)
			}

			condition := conditions.NewRolloutCondition(v1alpha1.RolloutProgressing, corev1.ConditionFalse, conditions.TimedOutReason, msg)
			_ = conditions.SetRolloutCondition(&newStatus, *condition)
		}
	}

	if c.rollout.Spec.Strategy.Canary != nil && replicasetutil.GetAvailableReplicaCountForReplicaSets(c.allRSs) >= defaults.GetReplicasOrDefault(c.rollout.Spec.Replicas) {
		availability := conditions.NewRolloutCondition(v1alpha1.RolloutAvailable, corev1.ConditionTrue, conditions.AvailableReason, conditions.AvailableMessage)
		conditions.SetRolloutCondition(&newStatus, *availability)
	} else {
		noAvailability := conditions.NewRolloutCondition(v1alpha1.RolloutAvailable, corev1.ConditionFalse, conditions.AvailableReason, conditions.NotAvailableMessage)
		conditions.SetRolloutCondition(&newStatus, *noAvailability)
	}

	// Move failure conditions of all replica sets in rollout conditions. For now,
	// only one failure condition is returned from getReplicaFailures.
	if replicaFailureCond := c.getReplicaFailures(c.allRSs, c.newRS); len(replicaFailureCond) > 0 {
		// There will be only one ReplicaFailure condition on the replica set.
		conditions.SetRolloutCondition(&newStatus, replicaFailureCond[0])
	} else {
		conditions.RemoveRolloutCondition(&newStatus, v1alpha1.RolloutReplicaFailure)
	}

	if conditions.RolloutCompleted(c.rollout, &newStatus) {
		// The event gets triggered in function promoteStable
		updateCompletedCond := conditions.NewRolloutCondition(v1alpha1.RolloutCompleted, corev1.ConditionTrue,
			conditions.RolloutCompletedReason, conditions.RolloutCompletedReason)
		conditions.SetRolloutCondition(&newStatus, *updateCompletedCond)
	} else {
		updateCompletedCond := conditions.NewRolloutCondition(v1alpha1.RolloutCompleted, corev1.ConditionFalse,
			conditions.RolloutCompletedReason, conditions.RolloutCompletedReason)
		conditions.SetRolloutCondition(&newStatus, *updateCompletedCond)
	}

	return newStatus
}

// persistRolloutStatus persists updates to rollout status. If no changes were made, it is a no-op
func (c *rolloutContext) persistRolloutStatus(newStatus *v1alpha1.RolloutStatus) error {
	ctx := context.TODO()
	logCtx := logutil.WithVersionFields(c.log, c.rollout)

	c.pauseContext.CalculatePauseStatus(newStatus)

	newStatus.Phase, newStatus.Message = rolloututil.CalculateRolloutPhase(c.rollout.Spec, *newStatus)

	c.rollout.Status = *newStatus
	_, err := c.rolloutManager.UpdateStatus(ctx, c.rollout.Annotations[annotations.ClusterCodeAnnotation], c.rollout)
	if err != nil {
		logCtx.Warningf("Error updating rollout: %v", err)
		return err
	}

	return nil
}

func (c *rolloutContext) getReplicaFailures(allRSs []*appsv1.ReplicaSet, newRS *appsv1.ReplicaSet) []v1alpha1.RolloutCondition {
	var errorConditions []v1alpha1.RolloutCondition
	if newRS != nil {
		for _, c := range newRS.Status.Conditions {
			if c.Type != appsv1.ReplicaSetReplicaFailure {
				continue
			}
			errorConditions = append(errorConditions, conditions.ReplicaSetToRolloutCondition(c))
		}
	}

	// Return failures for the new replica set over failures from old replica sets.
	if len(errorConditions) > 0 {
		return errorConditions
	}

	for i := range allRSs {
		rs := allRSs[i]
		if rs == nil {
			continue
		}

		for _, c := range rs.Status.Conditions {
			if c.Type != appsv1.ReplicaSetReplicaFailure {
				continue
			}
			errorConditions = append(errorConditions, conditions.ReplicaSetToRolloutCondition(c))
		}
	}
	return errorConditions
}

// resetRolloutStatus will reset the rollout status as if it is in a beginning of a new update
func (c *rolloutContext) resetRolloutStatus(newStatus *v1alpha1.RolloutStatus) {
	c.pauseContext.ClearPauseConditions()
	newStatus.CurrentStepIndex = replicasetutil.ResetCurrentStepIndex(c.rollout)
}

// shouldFullPromote returns a reason string explaining why a rollout should fully promote, marking
// the desired ReplicaSet as stable. Returns empty string if the rollout is in middle of update
func (c *rolloutContext) shouldFullPromote(newStatus v1alpha1.RolloutStatus) string {
	if c.rollout.Spec.Strategy.Canary != nil {
		if c.newRS.Status.AvailableReplicas >= defaults.GetReplicasOrDefault(c.rollout.Spec.Replicas) {
			return "Available replicas of new ReplicaSet are matched the rollout spec replicas"
		}
		if c.newRS == nil || c.newRS.Status.AvailableReplicas != defaults.GetReplicasOrDefault(c.rollout.Spec.Replicas) {
			return ""
		}

		_, currentStepIndex := replicasetutil.GetCurrentCanaryStep(c.rollout)
		stepCount := len(c.rollout.Spec.Strategy.Canary.Steps)
		completedAllSteps := stepCount == 0 || (currentStepIndex != nil && *currentStepIndex == int32(stepCount))
		if completedAllSteps {
			return fmt.Sprintf("Completed all %d canary steps", stepCount)
		}
	}

	return ""
}

// promoteStable will take appropriate action once we have promoted the current ReplicaSet as stable
// e.g. reset status conditions, etc...
// 当前 rs 成功到 stable 后的一些操作
func (c *rolloutContext) promoteStable(newStatus *v1alpha1.RolloutStatus, reason string) error {
	c.pauseContext.ClearPauseConditions()

	if c.rollout.Spec.Strategy.Canary != nil {
		stepCount := int32(len(c.rollout.Spec.Strategy.Canary.Steps))
		if stepCount > 0 {
			newStatus.CurrentStepIndex = &stepCount
		} else {
			newStatus.CurrentStepIndex = nil
		}
	}
	previousStableHash := newStatus.StableRS
	if previousStableHash != newStatus.CurrentPodHash {
		newStatus.StableRS = newStatus.CurrentPodHash
	}
	return nil
}
