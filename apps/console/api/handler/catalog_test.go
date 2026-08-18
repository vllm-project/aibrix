package handler

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/catalog"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

type acceleratorRegionCatalogStub struct {
	catalog.Catalog
	resources []catalog.Resource
	err       error
	opts      *catalog.ResourceListOptions
	calls     int
}

type regionFormatterStub struct {
	format func(*rmtypes.RegionSpec) string
}

func (f regionFormatterStub) FormatRegion(region *rmtypes.RegionSpec) string {
	if f.format != nil {
		return f.format(region)
	}
	if region == nil {
		return ""
	}
	if region.LambdaCloud != nil {
		return region.LambdaCloud.Region
	}
	if region.AWS != nil {
		return region.AWS.Region
	}
	if region.Kubernetes != nil {
		return strings.Join([]string{
			region.Kubernetes.Context,
			region.Kubernetes.Cluster,
			region.Kubernetes.Namespace,
		}, "/")
	}
	return ""
}

func (c *acceleratorRegionCatalogStub) ListResources(
	_ context.Context,
	opts *catalog.ResourceListOptions,
) ([]catalog.Resource, error) {
	c.calls++
	c.opts = opts
	return c.resources, c.err
}

func TestCatalogHandlerListAcceleratorRegions(t *testing.T) {
	resourceCatalog := &acceleratorRegionCatalogStub{
		resources: []catalog.Resource{
			catalogResourceWithSupply(
				*rmtypes.NewLambdaCloudRegion("US-West/USWEST2/Cloudnative/ai"),
				"NVIDIA-L20",
				"473",
			),
			catalogResourceWithSupply(
				*rmtypes.NewLambdaCloudRegion("US-East/USEAST1/Federation/default"),
				"NVIDIA-L20",
				"1870",
			),
			catalogResourceWithSupply(
				*rmtypes.NewLambdaCloudRegion("US-East/USEAST1/Federation/default"),
				"NVIDIA-L20",
				"1536",
			),
			catalogResourceWithSupply(
				*rmtypes.NewLambdaCloudRegion("US-Central/USCENTRAL1/Cloudnative/inference"),
				"NVIDIA-L20",
				"991",
			),
			catalogResourceWithSupply(
				*rmtypes.NewLambdaCloudRegion("US-South/USSOUTH1/Federation/default"),
				"NVIDIA-L20",
				"0",
			),
			catalogResourceWithSupply(
				*rmtypes.NewLambdaCloudRegion("US-North/USNORTH1/Cloudnative/default"),
				"NVIDIA-A30",
				"869",
			),
		},
	}
	h := NewCatalogHandler(resourceCatalog, regionFormatterStub{}, false)
	h.now = func() time.Time {
		return time.Date(2026, time.May, 30, 19, 37, 42, 0, time.FixedZone("PDT", -7*60*60))
	}

	regionsByAccelerator, err := h.listAcceleratorRegions(
		context.Background(),
		[]string{"NVIDIA L20"},
	)
	if err != nil {
		t.Fatalf("listAcceleratorRegions: %v", err)
	}
	regions := regionsByAccelerator["NVIDIA L20"]
	want := []string{
		"US-Central/USCENTRAL1/Cloudnative/inference",
		"US-East/USEAST1/Federation/default",
		"US-West/USWEST2/Cloudnative/ai",
	}
	if !reflect.DeepEqual(regions, want) {
		t.Fatalf("regions = %#v, want %#v", regions, want)
	}
	wantStart := time.Date(2026, time.May, 31, 1, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, time.May, 31, 2, 0, 0, 0, time.UTC)
	if resourceCatalog.opts == nil ||
		resourceCatalog.opts.StartTime == nil ||
		resourceCatalog.opts.EndTime == nil {
		t.Fatalf("resource list options = %#v, want time window", resourceCatalog.opts)
	}
	if !resourceCatalog.opts.StartTime.Equal(wantStart) {
		t.Fatalf("start time = %v, want %v", resourceCatalog.opts.StartTime, wantStart)
	}
	if !resourceCatalog.opts.EndTime.Equal(wantEnd) {
		t.Fatalf("end time = %v, want %v", resourceCatalog.opts.EndTime, wantEnd)
	}
}

func TestCatalogHandlerHidesUnsupportedCatalog(t *testing.T) {
	h := NewCatalogHandler(
		&acceleratorRegionCatalogStub{err: rmtypes.ErrNotImplemented},
		regionFormatterStub{},
		false,
	)

	regions, err := h.listAcceleratorRegions(
		context.Background(),
		[]string{"H100"},
	)
	if err != nil {
		t.Fatalf("listAcceleratorRegions: %v", err)
	}
	if len(regions["H100"]) != 0 {
		t.Fatalf("regions = %#v, want empty", regions)
	}
}

func TestCatalogHandlerPropagatesCatalogError(t *testing.T) {
	wantErr := errors.New("catalog unavailable")
	h := NewCatalogHandler(
		&acceleratorRegionCatalogStub{err: wantErr},
		regionFormatterStub{},
		false,
	)

	_, err := h.listAcceleratorRegions(
		context.Background(),
		[]string{"H100"},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

func TestCatalogHandlerDevModeHashesAcceleratorsToRegions(t *testing.T) {
	resourceCatalog := &acceleratorRegionCatalogStub{
		err: errors.New("real catalog should not be called in dev mode"),
	}
	h := NewCatalogHandler(
		resourceCatalog,
		regionFormatterStub{format: func(*rmtypes.RegionSpec) string { return "" }},
		true,
	)
	accelerators := []string{
		"NVIDIA-L20",
		"A100-PCIE-80GB",
		"AMD-MI308XHF",
		"Ascend910B3-64GB",
		"MLU590-M9DK",
		"Iluvatar-BI-V150",
		"jiuhuashan-PCIE-96T-16",
	}

	seen := make(map[string]struct{})
	regionsByAccelerator, err := h.listAcceleratorRegions(
		context.Background(),
		accelerators,
	)
	if err != nil {
		t.Fatalf("listAcceleratorRegions: %v", err)
	}
	for _, accelerator := range accelerators {
		regions := regionsByAccelerator[accelerator]
		if len(regions) == 0 || len(regions) > 3 {
			t.Fatalf("regions for %q = %v, want 1-3 regions", accelerator, regions)
		}
		for _, region := range regions {
			if !slices.ContainsFunc(devCatalogRegions, func(candidate devCatalogRegion) bool {
				return candidate.display == region
			}) {
				t.Fatalf("unexpected dev region %q for %q", region, accelerator)
			}
		}
		seen[strings.Join(regions, ",")] = struct{}{}
	}
	again, err := h.listAcceleratorRegions(context.Background(), accelerators)
	if err != nil {
		t.Fatalf("second listAcceleratorRegions: %v", err)
	}
	if !reflect.DeepEqual(again, regionsByAccelerator) {
		t.Fatalf("dev regions are not stable: first=%v second=%v", regionsByAccelerator, again)
	}

	if len(seen) < 2 {
		t.Fatalf("dev hash produced only one region combination: %v", seen)
	}
	if resourceCatalog.calls != 0 {
		t.Fatalf("real catalog calls = %d, want 0 in dev mode", resourceCatalog.calls)
	}
}

func TestCatalogHandlerDevModeReturnsNoRegionsForCPU(t *testing.T) {
	h := NewCatalogHandler(nil, regionFormatterStub{}, true)
	regions, err := h.listAcceleratorRegions(
		context.Background(),
		[]string{"CPU"},
	)
	if err != nil {
		t.Fatalf("listAcceleratorRegions(CPU): %v", err)
	}
	if len(regions) != 0 {
		t.Fatalf("CPU regions = %v, want empty", regions)
	}
}

func TestCatalogHandlerUsesBackendRegionDisplayFormat(t *testing.T) {
	resourceCatalog := &acceleratorRegionCatalogStub{
		resources: []catalog.Resource{
			catalogResourceWithSupply(
				rmtypes.RegionSpec{Kubernetes: &rmtypes.KubernetesRegion{
					Context:   "US-West",
					Cluster:   "USWEST2",
					Namespace: "ai",
				}},
				"NVIDIA-L20",
				"8",
			),
		},
	}
	h := NewCatalogHandler(
		resourceCatalog,
		regionFormatterStub{format: func(region *rmtypes.RegionSpec) string {
			if region == nil || region.Kubernetes == nil {
				return ""
			}
			return region.Kubernetes.Cluster
		}},
		false,
	)

	regions, err := h.listAcceleratorRegions(
		context.Background(),
		[]string{"NVIDIA-L20"},
	)
	if err != nil {
		t.Fatalf("listAcceleratorRegions: %v", err)
	}
	if !reflect.DeepEqual(regions["NVIDIA-L20"], []string{"USWEST2"}) {
		t.Fatalf("regions = %v, want [USWEST2]", regions)
	}
}

func TestResourceOffersAcceleratorUsesScheduledAndNestedSupply(t *testing.T) {
	resource := catalog.Resource{
		RegionResource: catalog.RegionResource{
			Overview: []catalog.RegionResourceItem{
				{
					NextLevel: []catalog.RegionResourceItem{
						{
							Stat: catalog.ResourceStat{
								Scheduled: &catalog.ScheduledResourceStatItem{
									Supply: catalog.ScheduledResourceItem{
										"2026-08-11T00:00:00Z": {
											"gpu": {"NVIDIA-L40S": "2"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if !resourceOffersAccelerator(resource, "NVIDIA L40S") {
		t.Fatal("expected nested scheduled supply to match accelerator")
	}
}

func TestNormalizeAcceleratorName(t *testing.T) {
	tests := map[string]string{
		"NVIDIA-H100-SXM4-80GB":  "H100SXM480GB",
		"NVIDIA H100 SXM 80G":    "H100SXM80G",
		"Tesla-T4":               "T4",
		"AMD-MI300X":             "MI300X",
		"AMD-MI308XHF":           "MI308XHF",
		"Ascend910b4-64G":        "910B464G",
		"Iluvatar-BI-V150":       "BIV150",
		"jiuhuashan-PCIE-96T-16": "JIUHUASHANPCIE96T16",
		"MLU590-M9DK":            "MLU590M9DK",
		"NVIDIA-L20":             "L20",
		"NVIDIA-A30":             "A30",
	}
	for input, want := range tests {
		if got := normalizeAcceleratorName(input); got != want {
			t.Errorf("normalizeAcceleratorName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAcceleratorNamesMatch(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  bool
	}{
		{left: "NVIDIA-L20", right: "NVIDIA L20", want: true},
		{left: "NVIDIA H100", right: "H100-SXM-80GB", want: true},
		{left: "H100-SXM5", right: "H100-SXM-80GB", want: true},
		{left: "V100-SXM2-16GB", right: "V100-SXM2-32GB", want: false},
		{left: "H100-PCIE-80GB", right: "H100-SXM-80GB", want: false},
		{left: "NVIDIA-A30", right: "NVIDIA-L20", want: false},
		{left: "NVIDIA-L20", right: "NVIDIA-L20-16", want: false},
		{left: "AMD-MI308X", right: "AMD-MI308XHF", want: false},
		{left: "Ascend910b4-64G", right: "ASCEND-910B4-64GB", want: true},
		{left: "Ascend910B3-64GB", right: "Ascend910B4-64GB", want: false},
		{left: "910B-376T", right: "910B-376T", want: true},
		{left: "910B-376T", right: "910C", want: false},
		{left: "MLU580-X6", right: "MLU590-M9DK", want: false},
		{left: "FF40", right: "FF45D", want: false},
		{left: "Iluvatar-BI-V150", right: "BI-V150", want: true},
		{left: "jiuhuashan", right: "jiuhuashan-96T", want: false},
		{left: "jiuhuashan-96T", right: "jiuhuashan-PCIE-96T-16", want: false},
	}
	for _, test := range tests {
		if got := acceleratorNamesMatch(test.left, test.right); got != test.want {
			t.Errorf(
				"acceleratorNamesMatch(%q, %q) = %v, want %v",
				test.left,
				test.right,
				got,
				test.want,
			)
		}
	}
}

func TestAcceleratorNamesMatchKnownCatalogSKUs(t *testing.T) {
	skus := []string{
		"A100-PCIE-40GB",
		"A100-PCIE-80GB",
		"A100-SXM-80GB",
		"A100-SXM4-40GB",
		"A800-PCIE-80GB",
		"A800-SXM-40GB",
		"A800-SXM-80GB",
		"AMD-MI308X",
		"AMD-MI308XHF",
		"910B-376T",
		"Ascend910B3-64GB",
		"Ascend910B4-32GB",
		"910C",
		"Ascend910b4-64G",
		"MLU580-X6",
		"FF36D",
		"FF40",
		"FF45D",
		"Iluvatar-BI-V150",
		"MLU590-M9DK",
		"NVIDIA-A10",
		"NVIDIA-A10G",
		"NVIDIA-A30",
		"NVIDIA-A40",
		"NVIDIA-B200",
		"NVIDIA-B40",
		"NVIDIA-GB300",
		"NVIDIA-H20",
		"NVIDIA-H30",
		"NVIDIA-L20",
		"NVIDIA-L20-16",
		"L4",
		"NVIDIA-L40",
		"NVIDIA-L40S",
		"NVIDIA-RTX-6000D",
		"Tesla-P4",
		"Tesla-P100-PCIE-16GB",
		"Tesla-T4",
		"Tesla-V100-PCIE-32GB",
		"Tesla-V100-SXM2-16GB",
		"Tesla-V100-SXM2-32GB",
		"jiuhuashan",
		"jiuhuashan-96T",
		"jiuhuashan-PCIE-96T-16",
		"jiuhuashan-sa-16",
	}

	for _, sku := range skus {
		t.Run(sku, func(t *testing.T) {
			if !acceleratorNamesMatch(sku, strings.ToLower(sku)) {
				t.Fatalf("expected %q to match its lowercase form", sku)
			}
			spaceSeparated := strings.ReplaceAll(sku, "-", " ")
			if !acceleratorNamesMatch(sku, spaceSeparated) {
				t.Fatalf("expected %q to match %q", sku, spaceSeparated)
			}
		})
	}
}

func TestAcceleratorNamesDoNotConflateKnownCatalogSKUs(t *testing.T) {
	tests := [][2]string{
		{"A100-PCIE-40GB", "A100-PCIE-80GB"},
		{"A100-PCIE-80GB", "A100-SXM-80GB"},
		{"A800-SXM-40GB", "A800-SXM-80GB"},
		{"AMD-MI308X", "AMD-MI308XHF"},
		{"Ascend910B3-64GB", "Ascend910B4-32GB"},
		{"Ascend910B4-32GB", "Ascend910b4-64G"},
		{"FF36D", "FF40"},
		{"FF40", "FF45D"},
		{"NVIDIA-A10", "NVIDIA-A10G"},
		{"NVIDIA-B200", "NVIDIA-B40"},
		{"NVIDIA-L20", "NVIDIA-L20-16"},
		{"NVIDIA-L40", "NVIDIA-L40S"},
		{"Tesla-V100-PCIE-32GB", "Tesla-V100-SXM2-32GB"},
		{"Tesla-V100-SXM2-16GB", "Tesla-V100-SXM2-32GB"},
		{"jiuhuashan", "jiuhuashan-96T"},
		{"jiuhuashan-96T", "jiuhuashan-PCIE-96T-16"},
		{"jiuhuashan-PCIE-96T-16", "jiuhuashan-sa-16"},
	}

	for _, pair := range tests {
		t.Run(pair[0]+"_vs_"+pair[1], func(t *testing.T) {
			if acceleratorNamesMatch(pair[0], pair[1]) {
				t.Fatalf("did not expect %q to match %q", pair[0], pair[1])
			}
		})
	}
}

func catalogResourceWithSupply(
	region rmtypes.RegionSpec,
	accelerator string,
	quantity string,
) catalog.Resource {
	return catalog.Resource{
		RegionResource: catalog.RegionResource{
			Region: &region,
			Overview: []catalog.RegionResourceItem{
				{
					Stat: catalog.ResourceStat{
						OnDemand: &catalog.ResourceStatItem{
							Supply: catalog.ResourceItem{
								"gpu": {accelerator: quantity},
							},
						},
					},
				},
			},
		},
	}
}
