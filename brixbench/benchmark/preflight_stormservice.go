package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type stormServiceRef struct {
	Namespace string
	Name      string
	Ready     string
	Age       string
}

type stormServiceList struct {
	Items []struct {
		Metadata struct {
			Name              string    `json:"name"`
			Namespace         string    `json:"namespace"`
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
		Spec struct {
			Replicas int `json:"replicas"`
		} `json:"spec"`
	} `json:"items"`
}

func ensureStormServicesCleared(ctx context.Context) error {
	services, err := listStormServices(ctx)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return nil
	}
	return fmt.Errorf(
		"existing StormService resources found outside the reset benchmark namespace:\n%s",
		formatStormServices(services),
	)
}

func listStormServices(ctx context.Context) ([]stormServiceRef, error) {
	cmd := exec.CommandContext(ctx, "bash", "-lc", "kubectl get stormservice -A -o json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "the server doesn't have a resource type") {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to list StormService resources: %v, output: %s", err, strings.TrimSpace(string(output)))
	}

	var list stormServiceList
	if err := json.Unmarshal(output, &list); err != nil {
		return nil, fmt.Errorf("failed to parse StormService list: %w", err)
	}

	services := make([]stormServiceRef, 0, len(list.Items))
	now := time.Now()
	for _, item := range list.Items {
		services = append(services, stormServiceRef{
			Namespace: item.Metadata.Namespace,
			Name:      item.Metadata.Name,
			Ready:     fmt.Sprintf("%d/%d", item.Status.ReadyReplicas, item.Spec.Replicas),
			Age:       humanDuration(now.Sub(item.Metadata.CreationTimestamp)),
		})
	}

	sort.Slice(services, func(i, j int) bool {
		if services[i].Namespace != services[j].Namespace {
			return services[i].Namespace < services[j].Namespace
		}
		return services[i].Name < services[j].Name
	})
	return services, nil
}

func formatStormServices(services []stormServiceRef) string {
	lines := make([]string, 0, len(services))
	for _, service := range services {
		lines = append(lines, fmt.Sprintf("- %s/%s ready=%s age=%s", service.Namespace, service.Name, service.Ready, service.Age))
	}
	return strings.Join(lines, "\n")
}

func humanDuration(duration time.Duration) string {
	if duration < time.Minute {
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	}
	if duration < time.Hour {
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	}
	if duration < 24*time.Hour {
		return fmt.Sprintf("%dh", int(duration.Hours()))
	}
	return fmt.Sprintf("%dd", int(duration.Hours()/24))
}
