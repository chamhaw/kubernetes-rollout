package queue

import (
	"time"

	"k8s.io/client-go/util/workqueue"
)

// DefaultRolloutsRateLimiter is the default queue rate limiter.
// Similar to workqueue.DefaultControllerRateLimiter() but the max limit is 10 seconds instead of 16 minutes
func DefaultRolloutsRateLimiter() workqueue.RateLimiter {
	return workqueue.NewItemExponentialFailureRateLimiter(time.Millisecond, 10*time.Second)
}
