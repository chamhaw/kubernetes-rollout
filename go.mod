module github.com/chamhaw/kubernetes-rollout

go 1.19


replace (
	github.com/chamhaw/kubernetes-rollout/api => ./api
	github.com/chamhaw/kubernetes-rollout/rollout => ./rollout
)

require (
	github.com/chamhaw/kubernetes-rollout/api latest
	github.com/chamhaw/kubernetes-rollout/rollout latest
)