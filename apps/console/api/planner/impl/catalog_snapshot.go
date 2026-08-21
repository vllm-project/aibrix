/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package impl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/vllm-project/aibrix/apps/console/api/metrics"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/catalog"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"github.com/vllm-project/aibrix/apps/console/api/store/models"
	"gorm.io/datatypes"
	"k8s.io/klog/v2"
)

const (
	catalogSnapshotWindow     = time.Hour
	catalogCollectionInterval = 20 * time.Minute
	catalogCollectionTimeout  = 30 * time.Second
)

type catalogMetricKey struct {
	provider     string
	view         string
	region       string
	capacity     string
	offering     string
	resourceType string
	resourceName string
}

func (w *planningLoop) collectCatalogSnapshots(ctx context.Context, cycleTime time.Time) {
	p := w.planner
	if p.store == nil || p.backend == nil {
		return
	}

	windowStart := cycleTime.UTC().Truncate(catalogSnapshotWindow)
	windowEnd := windowStart.Add(catalogSnapshotWindow)
	opts := &catalog.ResourceListOptions{StartTime: &windowStart, EndTime: &windowEnd}

	if resources, err := p.backend.ListResources(ctx, opts); err != nil {
		w.logCatalogQueryFailure("resource", err)
	} else {
		w.persistAndEmitCatalogView(ctx, models.CatalogViewResource, windowStart, windowEnd, resources, resources)
	}

	if predictions, err := p.backend.ListResourcePredictions(ctx, opts); err != nil {
		w.logCatalogQueryFailure("prediction", err)
	} else {
		predictedResources := make([]catalog.Resource, 0, len(predictions))
		for _, resource := range predictions {
			predictedResources = append(predictedResources, resource)
		}
		w.persistAndEmitCatalogView(ctx, models.CatalogViewPrediction, windowStart, windowEnd, predictions, predictedResources)
	}
}

func (w *planningLoop) persistAndEmitCatalogView(
	ctx context.Context,
	view models.CatalogViewType,
	windowStart, windowEnd time.Time,
	payloadValue any,
	resources []catalog.Resource,
) {
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		w.recordCatalogCollectionFailure(string(view)+"_encode_failed", err)
		return
	}

	snapshot := &models.CatalogSnapshot{
		Provider:    string(w.planner.prov.Type()),
		ViewType:    view,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		Payload:     datatypes.JSON(payload),
	}
	if err := w.planner.store.UpsertCatalogSnapshot(ctx, snapshot); err != nil {
		w.recordCatalogCollectionFailure(string(view)+"_persist_failed", err)
		return
	}

	emitCatalogResourceMetrics(view, resources)
}

func (w *planningLoop) logCatalogQueryFailure(view string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, rmtypes.ErrNotImplemented) || errors.Is(err, rmtypes.ErrUnsupportedCatalog) {
		klog.V(4).Infof("[planner] catalog %s query skipped: %v", view, err)
		return
	}
	klog.Warningf("[planner] catalog %s query skipped: %v", view, err)
}

func (w *planningLoop) recordCatalogCollectionFailure(reason string, err error) {
	metrics.Emitter.Counter(metricConsolePlannerError, 1,
		metrics.T("method", "catalog_snapshot"),
		metrics.T("reason", reason),
	)
	klog.Warningf("[planner] catalog snapshot %s: %v", reason, err)
}

func emitCatalogResourceMetrics(view models.CatalogViewType, resources []catalog.Resource) {
	values := make(map[catalogMetricKey]float64)
	for _, resource := range resources {
		provider := string(resource.Provider)
		region := resource.Region.String()
		for _, item := range resource.Overview {
			collectCatalogItemMetrics(values, provider, string(view), region, item)
		}
	}

	for key, value := range values {
		emitCatalogGauge(key, float32(value))
	}
}

func emitCatalogGauge(key catalogMetricKey, value float32) {
	metrics.Emitter.Gauge(
		fmt.Sprintf("console.planner.catalog.%s.%s", key.view, key.capacity),
		value,
		metrics.T("provider", key.provider),
		metrics.T("region", key.region),
		metrics.T("offering", key.offering),
		metrics.T("resource_type", key.resourceType),
		metrics.T("resource_name", key.resourceName),
	)
}

func collectCatalogItemMetrics(values map[catalogMetricKey]float64, provider, view, region string, item catalog.RegionResourceItem) {
	if item.Stat.OnDemand != nil {
		collectResourceStatItem(values, provider, view, region, "on_demand", *item.Stat.OnDemand)
	}
	if item.Stat.Spot != nil {
		collectResourceStatItem(values, provider, view, region, "spot", *item.Stat.Spot)
	}
	if item.Stat.Scheduled != nil {
		collectScheduledResourceStatItem(values, provider, view, region, "scheduled", *item.Stat.Scheduled)
	}
	if item.Stat.OnDemand == nil && item.Stat.Spot == nil && item.Stat.Scheduled == nil {
		for _, next := range item.NextLevel {
			collectCatalogItemMetrics(values, provider, view, region, next)
		}
	}
}

func collectResourceStatItem(values map[catalogMetricKey]float64, provider, view, region, offering string, stat catalog.ResourceStatItem) {
	collectResourceItem(values, provider, view, region, offering, "allocated", stat.Allocated)
	collectResourceItem(values, provider, view, region, offering, "supply", stat.Supply)
	collectResourceItem(values, provider, view, region, offering, "allocatable", stat.Allocatable)
}

func collectScheduledResourceStatItem(values map[catalogMetricKey]float64, provider, view, region, offering string, stat catalog.ScheduledResourceStatItem) {
	collectScheduledResourceItem(values, provider, view, region, offering, "allocated", stat.Allocated)
	collectScheduledResourceItem(values, provider, view, region, offering, "supply", stat.Supply)
	collectScheduledResourceItem(values, provider, view, region, offering, "allocatable", stat.Allocatable)
}

func collectScheduledResourceItem(values map[catalogMetricKey]float64, provider, view, region, offering, capacity string, scheduled catalog.ScheduledResourceItem) {
	for _, item := range scheduled {
		collectResourceItem(values, provider, view, region, offering, capacity, item)
	}
}

func collectResourceItem(values map[catalogMetricKey]float64, provider, view, region, offering, capacity string, item catalog.ResourceItem) {
	for resourceType, resources := range item {
		for resourceName, rawValue := range resources {
			value, err := strconv.ParseFloat(rawValue, 64)
			if err != nil {
				klog.Warningf("[planner] skip non-numeric catalog resource %s/%s=%q", resourceType, resourceName, rawValue)
				continue
			}
			key := catalogMetricKey{
				provider:     provider,
				view:         view,
				region:       region,
				capacity:     capacity,
				offering:     offering,
				resourceType: resourceType,
				resourceName: resourceName,
			}
			values[key] += value
		}
	}
}
