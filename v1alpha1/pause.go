package v1

import (
	"github.com/chamhaw/kubernetes-rollout-api/v1alpha1"
	"time"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	timeutil "github.com/chamhaw/kubernetes-rollout/utils/time"
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
	// ???????????? unpause ???????????? pause condition ??? spec.paused = false????????????????????????ControllerPause??? true????????????????????? pause step
	// ?????? pause ??? rolling??? ????????????pause condition??? ?????????????????????unpause??????
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

// ??????
func (c *rolloutContext) checkEnqueueRolloutDuringWait(startTime metav1.Time, durationInSeconds int32) {
	now := timeutil.MetaNow()
	expiredTime := startTime.Add(time.Duration(durationInSeconds) * time.Second)
	nextResync := now.Add(c.resyncPeriod)
	// ????????????????????????????????????????????????????????? re-sync ??????????????? rollout ??????????????????
	if nextResync.After(expiredTime) && expiredTime.After(now.Time) {
		// expiredTime - now , ?????????????????????
		timeRemaining := expiredTime.Sub(now.Time)
		c.log.Infof("Enqueueing Rollout in %s seconds", timeRemaining.String())
		c.enqueueRolloutAfter(c.rollout, timeRemaining)
	}
}
