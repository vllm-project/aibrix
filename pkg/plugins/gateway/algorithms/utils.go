package routingalgorithms

import (
	"fmt"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"
)

// podServerKey represents a pod with specific port
type podServerKey struct {
	pod  *v1.Pod
	port int
}

func (k podServerKey) String() string {
	if k.port == 0 {
		return fmt.Sprintf("%s/%s", k.pod.Namespace, k.pod.Name)
	}
	return fmt.Sprintf("%s/%s/%d", k.pod.Namespace, k.pod.Name, k.port)
}

// revert podServerKey.String to (namespace, name, port)
func parseServerKey(key string) (namespace string, name string, port int) {
	parts := strings.Split(key, "/")
	if len(parts) == 2 {
		namespace = parts[0]
		name = parts[1]
		port = 0
	} else if len(parts) == 3 {
		namespace = parts[0]
		name = parts[1]
		var err error
		if port, err = strconv.Atoi(parts[2]); err != nil {
			klog.ErrorS(err, "failed to parse port from server key", "key", key)
		}
	}
	return
}
