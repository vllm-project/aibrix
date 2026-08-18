package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"k8s.io/klog/v2"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/catalog"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

const catalogRegionsPath = "/api/v1/catalog/regions"

var (
	acceleratorCapacityPattern = regexp.MustCompile(`^\d+(?:GB|G)$`)
	acceleratorVariantPattern  = regexp.MustCompile(`^(?:SXM\d*|PCIE|NVL)$`)
)

var acceleratorVendorPrefixes = []string{
	"NVIDIA",
	"TESLA",
	"AMD",
	"ASCEND",
	"HUAWEI",
	"ILUVATAR",
}

type devCatalogRegion struct {
	spec    rmtypes.RegionSpec
	display string
}

var devCatalogRegions = []devCatalogRegion{
	newDevCatalogRegion("US-Central", "USCENTRAL1", "Cloudnative", "inference"),
	newDevCatalogRegion("US-East", "USEAST1", "Federation", "default"),
	newDevCatalogRegion("US-West", "USWEST1", "Federation", "batch"),
	newDevCatalogRegion("US-West", "USWEST2", "Cloudnative", "ai"),
}

func newDevCatalogRegion(zone, dc, physicalCluster, logicalCluster string) devCatalogRegion {
	cloudRegion := strings.ToLower(dc)
	return devCatalogRegion{
		spec: rmtypes.RegionSpec{
			AWS:         &rmtypes.AWSRegion{Region: cloudRegion},
			LambdaCloud: &rmtypes.LambdaCloudRegion{Region: cloudRegion},
			Kubernetes: &rmtypes.KubernetesRegion{
				Context:   zone,
				Cluster:   physicalCluster,
				Namespace: logicalCluster,
			},
		},
		display: dc,
	}
}

type CatalogHandler struct {
	catalog         catalog.Catalog
	regionFormatter plannerapi.RegionFormatter
	devMode         bool
	now             func() time.Time
}

type catalogRegionsRequest struct {
	Accelerators []string `json:"accelerators"`
}

type catalogRegionsResponse struct {
	Regions map[string][]string `json:"regions"`
}

func NewCatalogHandler(
	resourceCatalog catalog.Catalog,
	regionFormatter plannerapi.RegionFormatter,
	devMode bool,
) *CatalogHandler {
	return &CatalogHandler{
		catalog:         resourceCatalog,
		regionFormatter: regionFormatter,
		devMode:         devMode,
		now:             time.Now,
	}
}

func (h *CatalogHandler) RegisterRoutes(mux *runtime.ServeMux) error {
	if err := mux.HandlePath(http.MethodPost, catalogRegionsPath, h.handleListAcceleratorRegions); err != nil {
		return fmt.Errorf("register %s %s: %w", http.MethodPost, catalogRegionsPath, err)
	}
	return nil
}

func (h *CatalogHandler) handleListAcceleratorRegions(
	w http.ResponseWriter,
	r *http.Request,
	_ map[string]string,
) {
	var req catalogRegionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	regions, err := h.listAcceleratorRegions(r.Context(), req.Accelerators)
	if err != nil {
		klog.Errorf("list catalog regions for accelerators: %v", err)
		http.Error(w, "resource catalog unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(catalogRegionsResponse{Regions: regions}); err != nil {
		klog.Errorf("write catalog accelerator regions response: %v", err)
	}
}

func (h *CatalogHandler) listAcceleratorRegions(
	ctx context.Context,
	accelerators []string,
) (map[string][]string, error) {
	uniqueAccelerators := make([]string, 0, len(accelerators))
	seen := make(map[string]struct{}, len(accelerators))
	regionsByAccelerator := make(map[string][]string, len(accelerators))
	for _, accelerator := range accelerators {
		accelerator = strings.TrimSpace(accelerator)
		if accelerator == "" || strings.EqualFold(accelerator, "CPU") {
			continue
		}
		key := strings.ToUpper(accelerator)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		uniqueAccelerators = append(uniqueAccelerators, accelerator)
		regionsByAccelerator[accelerator] = []string{}
	}
	if len(uniqueAccelerators) == 0 {
		return regionsByAccelerator, nil
	}
	if h.devMode {
		for _, accelerator := range uniqueAccelerators {
			regionsByAccelerator[accelerator] = devRegionsForAccelerator(
				accelerator,
				h.regionFormatter,
			)
		}
		return regionsByAccelerator, nil
	}
	if h.catalog == nil {
		return regionsByAccelerator, nil
	}

	now := time.Now
	if h.now != nil {
		now = h.now
	}
	endTime := now().UTC().Truncate(time.Hour)
	startTime := endTime.Add(-time.Hour)
	resources, err := h.catalog.ListResources(ctx, &catalog.ResourceListOptions{
		StartTime: &startTime,
		EndTime:   &endTime,
	})
	if err != nil {
		if errors.Is(err, rmtypes.ErrNotImplemented) || errors.Is(err, rmtypes.ErrUnsupportedCatalog) {
			return regionsByAccelerator, nil
		}
		return nil, err
	}

	regionSets := make(map[string]map[string]struct{}, len(uniqueAccelerators))
	for _, accelerator := range uniqueAccelerators {
		regionSets[accelerator] = make(map[string]struct{})
	}
	for _, resource := range resources {
		if resource.Region == nil {
			continue
		}
		region := formatRegion(h.regionFormatter, resource.Region)
		if region == "" || region == rmtypes.RegionUnknown {
			continue
		}
		for _, accelerator := range uniqueAccelerators {
			if resourceOffersAccelerator(resource, accelerator) {
				regionSets[accelerator][region] = struct{}{}
			}
		}
	}

	for _, accelerator := range uniqueAccelerators {
		regions := make([]string, 0, len(regionSets[accelerator]))
		for region := range regionSets[accelerator] {
			regions = append(regions, region)
		}
		sort.Strings(regions)
		regionsByAccelerator[accelerator] = regions
	}
	return regionsByAccelerator, nil
}

func devRegionsForAccelerator(
	accelerator string,
	formatter plannerapi.RegionFormatter,
) []string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(normalizeAcceleratorName(accelerator)))
	value := hash.Sum32()
	count := 1 + int(value%3)
	start := int((value / 3) % uint32(len(devCatalogRegions)))

	regions := make([]string, 0, count)
	for offset := 0; offset < count; offset++ {
		region := &devCatalogRegions[(start+offset)%len(devCatalogRegions)]
		display := formatRegion(formatter, &region.spec)
		if display == "" {
			display = region.display
		}
		if display != "" {
			regions = append(regions, display)
		}
	}
	sort.Strings(regions)
	return regions
}

func formatRegion(
	formatter plannerapi.RegionFormatter,
	region *rmtypes.RegionSpec,
) string {
	if formatter == nil || region == nil {
		return ""
	}
	return formatter.FormatRegion(region)
}

func resourceOffersAccelerator(resource catalog.Resource, accelerator string) bool {
	for _, item := range resource.Overview {
		if regionResourceItemOffersAccelerator(item, accelerator) {
			return true
		}
	}
	for _, items := range resource.Breakdown {
		for _, item := range items {
			if regionResourceItemOffersAccelerator(item, accelerator) {
				return true
			}
		}
	}
	return false
}

func regionResourceItemOffersAccelerator(item catalog.RegionResourceItem, accelerator string) bool {
	for _, stat := range []*catalog.ResourceStatItem{item.Stat.OnDemand, item.Stat.Spot} {
		if stat != nil && resourceItemOffersAccelerator(stat.Supply, accelerator) {
			return true
		}
	}
	if item.Stat.Scheduled != nil {
		for _, supply := range item.Stat.Scheduled.Supply {
			if resourceItemOffersAccelerator(supply, accelerator) {
				return true
			}
		}
	}
	for _, next := range item.NextLevel {
		if regionResourceItemOffersAccelerator(next, accelerator) {
			return true
		}
	}
	return false
}

func resourceItemOffersAccelerator(item catalog.ResourceItem, accelerator string) bool {
	for resourceType, resources := range item {
		if !strings.EqualFold(resourceType, "gpu") {
			continue
		}
		for name, quantity := range resources {
			available, err := strconv.ParseFloat(quantity, 64)
			if err == nil && available > 0 && acceleratorNamesMatch(name, accelerator) {
				return true
			}
		}
	}
	return false
}

func normalizeAcceleratorName(value string) string {
	return strings.Join(acceleratorTokens(value), "")
}

func acceleratorNamesMatch(left, right string) bool {
	if normalizeAcceleratorName(left) == normalizeAcceleratorName(right) {
		return true
	}

	leftDetails := parseAcceleratorDetails(left)
	rightDetails := parseAcceleratorDetails(right)
	if leftDetails.model == "" || leftDetails.model != rightDetails.model {
		return false
	}
	if leftDetails.capacity != "" &&
		rightDetails.capacity != "" &&
		leftDetails.capacity != rightDetails.capacity {
		return false
	}
	if !acceleratorVariantsMatch(leftDetails.variant, rightDetails.variant) {
		return false
	}
	return slices.Equal(leftDetails.qualifiers, rightDetails.qualifiers)
}

type acceleratorDetails struct {
	model      string
	capacity   string
	variant    string
	qualifiers []string
}

func parseAcceleratorDetails(value string) acceleratorDetails {
	var details acceleratorDetails
	tokens := acceleratorTokens(value)
	if len(tokens) == 0 {
		return details
	}
	details.model = tokens[0]
	for _, token := range tokens[1:] {
		switch {
		case details.capacity == "" && acceleratorCapacityPattern.MatchString(token):
			details.capacity = strings.TrimSuffix(token, "B")
		case details.variant == "" && acceleratorVariantPattern.MatchString(token):
			details.variant = token
		default:
			details.qualifiers = append(details.qualifiers, token)
		}
	}
	return details
}

func acceleratorTokens(value string) []string {
	tokens := strings.FieldsFunc(strings.ToUpper(strings.TrimSpace(value)), func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	if len(tokens) == 0 {
		return nil
	}

	for _, prefix := range acceleratorVendorPrefixes {
		switch {
		case tokens[0] == prefix:
			tokens = tokens[1:]
		case strings.HasPrefix(tokens[0], prefix) && len(tokens[0]) > len(prefix):
			tokens[0] = strings.TrimPrefix(tokens[0], prefix)
		}
		if len(tokens) == 0 {
			return nil
		}
	}
	return tokens
}

func acceleratorVariantsMatch(left, right string) bool {
	if left == "" || right == "" || left == right {
		return true
	}
	return left == "SXM" && strings.HasPrefix(right, "SXM") ||
		right == "SXM" && strings.HasPrefix(left, "SXM")
}
