package v1

import (
	"github.com/chamhaw/kubernetes-rollout/api/v1alpha1"
	"time"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	timeutil "github.com/chamhaw/kubernetes-rollout/rollout/utils/time"
)

type pauseContext struct {
	rollout *v1alpha1.Rollout
	log     *log.Entry

	addPauseReasons      []v1alpha1.PauseReason
	removePauseReasons   []v1alpha1.PauseReason
	clearPauseConditions bool
}

func (pCtx *pauseContext) HasAddPause() bool {
	return len(pCtx.addPauseReasons) > 0
}

func (pCtx *pauseContext) AddPauseCondition(reason v1alpha1.PauseReason) {
	pCtx.addPauseReasons = append(pCtx.addPauseReasons, reason)
}

func (pCtx *pauseContext) ClearPauseConditions() {
	pCtx.clearPauseConditions = true
}

func (pCtx *pauseContext) CalculatePauseStatus(newStatus *v1alpha1.RolloutStatus) {
	now := timeutil.MetaNow()

	if pCtx.clearPauseConditions {
		newStatus.PauseConditions = []v1alpha1.PauseCondition{}
		return
	}

	controllerPause := pCtx.rollout.Status.ControllerPause
	statusToRemove := map[v1alpha1.PauseReason]bool{}
	for i := range pCtx.removePauseReasons {
		statusToRemove[pCtx.removePauseReasons[i]] = true
	}

	var newPauseConditions []v1alpha1.PauseCondition
	pauseAlreadyExists := map[v1alpha1.PauseReason]bool{}
	for _, cond := range pCtx.rollout.Status.PauseConditions {
		if remove := statusToRemove[cond.Reason]; !remove {
			newPauseConditions = append(newPauseConditions, cond)
		}
		pauseAlreadyExists[cond.Reason] = true
	}

	for i := range pCtx.addPauseReasons {
		reason := pCtx.addPauseReasons[i]
		if exists := pauseAlreadyExists[reason]; !exists {
			pCtx.log.Infof("Adding pause reason %s with start time %s", reason, now.UTC().Format(time.RFC3339))
			cond := v1alpha1.PauseCondition{
				Reason:    reason,
				StartTime: now,
			}
			newPauseConditions = append(newPauseConditions, cond)
			controllerPause = true
		}
	}

	if len(newPauseConditions) == 0 {
		return
	}
	newStatus.ControllerPause = controllerPause
	newStatus.PauseConditions = newPauseConditions
}

func getPauseCondition(rollout *v1alpha1.Rollout, reason v1alpha1.PauseReason) *v1alpha1.PauseCondition {
	for i := range rollout.Status.PauseConditions {
		cond := rollout.Status.PauseConditions[i]
		if cond.Reason == reason {
			return &cond
		}
	}
	return nil
}

func (pCtx *pauseContext) CompletedCanaryPauseStep(pause v1alpha1.RolloutPause) bool {
	rollout := pCtx.rollout
	pauseCondition := getPauseCondition(rollout, v1alpha1.PauseReasonCanaryPauseStep)
	// 用户手动 unpause 时，移除 pause condition 和 spec.paused = false（如有），要保持ControllerPause为 true，这样才能完成 pause step
	// 系统 pause 了 rolling， 但是没有pause condition， 说明时用户手动unpause了。
	if rollout.Status.ControllerPause && pauseCondition == nil {
		pCtx.log.Info("Rollout has been unpaused")
		return true
	} else if pause.Duration != nil {
		now := timeutil.MetaNow()
		if pauseCondition != nil {
			expiredTime := pauseCondition.StartTime.Add(time.Duration(pause.DurationSeconds()) * time.Second)
			if now.After(expiredTime) {
				pCtx.log.Info("Rollout has waited the duration of the pause step")
				return true
			}
		}
	}
	return false
}

// 检查
func (c *rolloutContext) checkEnqueueRolloutDuringWait(startTime metav1.Time, durationInSeconds int32) {
	now := timeutil.MetaNow()
	expiredTime := startTime.Add(time.Duration(durationInSeconds) * time.Second)
	nextResync := now.Add(c.resyncPeriod)
	// 如果还没到过期时间，但是过期时间在下次 re-sync 之前，则把 rollout 丢进延时队列
	if nextResync.After(expiredTime) && expiredTime.After(now.Time) {
		// expiredTime - now , 还差这么多时间
		timeRemaining := expiredTime.Sub(now.Time)
		c.log.Infof("Enqueueing Rollout in %s seconds", timeRemaining.String())
		c.enqueueRolloutAfter(c.rollout, timeRemaining)
	}
}
