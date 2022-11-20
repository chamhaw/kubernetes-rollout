package hash

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/rand"
)

// ComputePodTemplateHash podTemplate json序列化后 计算hash
func ComputePodTemplateHash(template *corev1.PodTemplateSpec, collisionCount *int32) string {
	podTemplateSpecHasher := fnv.New32a()
	stepsBytes, err := json.Marshal(template)
	if err != nil {
		panic(err)
	}
	_, err = podTemplateSpecHasher.Write(stepsBytes)
	if err != nil {
		panic(err)
	}
	if collisionCount != nil {
		collisionCountBytes := make([]byte, 8)
		binary.LittleEndian.PutUint32(collisionCountBytes, uint32(*collisionCount))
		_, err = podTemplateSpecHasher.Write(collisionCountBytes)
		if err != nil {
			panic(err)
		}
	}
	return rand.SafeEncodeString(fmt.Sprint(podTemplateSpecHasher.Sum32()))
}

// JsonBasedComputeHash  json序列化后 计算hash
func JsonBasedComputeHash(item json.Marshaler, collisionCount *int32) string {
	hasher := fnv.New32a()
	stepsBytes, err := json.Marshal(item)
	if err != nil {
		panic(err)
	}
	_, err = hasher.Write(stepsBytes)
	if err != nil {
		panic(err)
	}
	if collisionCount != nil {
		collisionCountBytes := make([]byte, 8)
		binary.LittleEndian.PutUint32(collisionCountBytes, uint32(*collisionCount))
		_, err = hasher.Write(collisionCountBytes)
		if err != nil {
			panic(err)
		}
	}
	return rand.SafeEncodeString(fmt.Sprint(hasher.Sum32()))
}
