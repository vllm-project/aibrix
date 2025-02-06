package routingalgorithms

import (
	"fmt"
)

const podMetricPort = "8000"

func getPodAddress(podIP string) (string, error) {
	if podIP == "" {
		return "", fmt.Errorf("no pods to forward request")
	}
	return fmt.Sprintf("%v:%v", podIP, podMetricPort), nil
}
