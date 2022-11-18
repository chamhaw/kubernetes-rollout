package v1alpha1

import (
	"encoding/json"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"testing"
)

func TestEnvVar(t *testing.T) {
	arr := `[
              {
                "name": "UB_APP_NAME",
                "value": "urbanic-app"
              }
            ]`

	var env []corev1.EnvVar
	err := json.Unmarshal([]byte(arr), &env)
	if err != nil {
		fmt.Println("err", err)
	}
	fmt.Println(env)
}

func TestProbe(t *testing.T) {
	str := `{
              "failureThreshold": 30,
              "periodSeconds": 10,
              "tcpSocket": {
                "port": 80
              },
              "timeoutSeconds": 1,
              "successThreshold": 1,
              "initialDelaySeconds": 20
            }`

	var probe corev1.Probe
	err := json.Unmarshal([]byte(str), &probe)
	if err != nil {
		fmt.Println("err", err)
	}
	fmt.Println(probe)
}

func TestResources(t *testing.T) {

}
