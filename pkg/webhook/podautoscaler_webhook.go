/*
Copyright 2024 The Aibrix Team.

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

package webhook

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var podautoscalerlog = logf.Log.WithName("podautoscaler-resource")

const (
	maxMetricWindowSeconds            = int64(3600)
	defaultObserveWindowSeconds       = int64(180)
	defaultPanicWindowSeconds         = int64(60)
	scheduledBoundsValidationWeekDays = 7
)

type scheduledBoundsCron struct {
	minute   int
	hours    [24]bool
	weekdays [7]bool
}

type scheduledBoundsWindow struct {
	start time.Time
	end   time.Time
}

// SetupPodAutoscalerWebhookWithManager registers the webhook for PodAutoscaler in the manager.
func SetupPodAutoscalerWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&autoscalingv1alpha1.PodAutoscaler{}).
		WithValidator(&PodAutoscalerCustomValidator{}).
		WithDefaulter(&PodAutoscalerCustomDefaulter{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// +kubebuilder:webhook:path=/mutate-autoscaling-aibrix-ai-v1alpha1-podautoscaler,mutating=true,failurePolicy=ignore,sideEffects=None,groups=autoscaling.aibrix.ai,resources=podautoscalers,verbs=create;update,versions=v1alpha1,name=mpodautoscaler-v1alpha1.kb.io,admissionReviewVersions=v1

// PodAutoscalerCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind PodAutoscaler when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type PodAutoscalerCustomDefaulter struct {
}

var _ webhook.CustomDefaulter = &PodAutoscalerCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind PodAutoscaler.
func (d *PodAutoscalerCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	podautoscaler, ok := obj.(*autoscalingv1alpha1.PodAutoscaler)

	if !ok {
		return fmt.Errorf("expected an PodAutoscaler object but got %T", obj)
	}
	podautoscalerlog.Info("Defaulting for PodAutoscaler", "name", podautoscaler.GetName())
	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-autoscaling-aibrix-ai-v1alpha1-podautoscaler,mutating=false,failurePolicy=ignore,sideEffects=None,groups=autoscaling.aibrix.ai,resources=podautoscalers,verbs=create;update,versions=v1alpha1,name=vpodautoscaler-v1alpha1.kb.io,admissionReviewVersions=v1

// PodAutoscalerCustomValidator struct is responsible for validating the PodAutoscaler resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PodAutoscalerCustomValidator struct {
}

var _ webhook.CustomValidator = &PodAutoscalerCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type PodAutoscaler.
func (v *PodAutoscalerCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	podautoscaler, ok := obj.(*autoscalingv1alpha1.PodAutoscaler)
	if !ok {
		return nil, fmt.Errorf("expected a PodAutoscaler object but got %T", obj)
	}
	podautoscalerlog.Info("Validation for PodAutoscaler upon creation", "name", podautoscaler.GetName())
	return nil, v.validatePodAutoscaler(podautoscaler)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PodAutoscaler.
func (v *PodAutoscalerCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	podautoscaler, ok := newObj.(*autoscalingv1alpha1.PodAutoscaler)
	if !ok {
		return nil, fmt.Errorf("expected a PodAutoscaler object for the newObj but got %T", newObj)
	}
	podautoscalerlog.Info("Validation for PodAutoscaler upon update", "name", podautoscaler.GetName())

	return nil, v.validatePodAutoscaler(podautoscaler)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PodAutoscaler.
func (v *PodAutoscalerCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	podautoscaler, ok := obj.(*autoscalingv1alpha1.PodAutoscaler)
	if !ok {
		return nil, fmt.Errorf("expected a PodAutoscaler object but got %T", obj)
	}
	podautoscalerlog.Info("Validation for PodAutoscaler upon deletion", "name", podautoscaler.GetName())
	return nil, nil
}

// validatePodAutoscaler performs all spec validations.
func (v *PodAutoscalerCustomValidator) validatePodAutoscaler(pa *autoscalingv1alpha1.PodAutoscaler) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// 1. Validate ScaleTargetRef
	targetRef := pa.Spec.ScaleTargetRef
	targetRefPath := specPath.Child("scaleTargetRef")
	if targetRef.Name == "" {
		allErrs = append(allErrs, field.Required(targetRefPath.Child("name"), "must be set"))
	}
	if targetRef.Kind == "" {
		allErrs = append(allErrs, field.Required(targetRefPath.Child("kind"), "must be set"))
	}

	// 2. Validate Replica Bounds
	if pa.Spec.MinReplicas != nil && pa.Spec.MaxReplicas < *pa.Spec.MinReplicas {
		minPath := specPath.Child("minReplicas")
		maxPath := specPath.Child("maxReplicas")
		allErrs = append(allErrs,
			field.Invalid(minPath, pa.Spec.MinReplicas, "cannot be greater than maxReplicas"),
			field.Invalid(maxPath, pa.Spec.MaxReplicas, "cannot be less than minReplicas"),
		)
	}

	allErrs = append(allErrs, validateScheduledBounds(pa, specPath)...)
	allErrs = append(allErrs, validateMetricWindows(pa, specPath)...)

	// 3. Validate ScalingStrategy
	validStrategies := map[autoscalingv1alpha1.ScalingStrategyType]bool{
		autoscalingv1alpha1.HPA: true,
		autoscalingv1alpha1.KPA: true,
		autoscalingv1alpha1.APA: true,
	}
	if !validStrategies[pa.Spec.ScalingStrategy] {
		strategyPath := specPath.Child("scalingStrategy")
		allErrs = append(allErrs, field.NotSupported(strategyPath, pa.Spec.ScalingStrategy, []string{
			string(autoscalingv1alpha1.HPA),
			string(autoscalingv1alpha1.KPA),
			string(autoscalingv1alpha1.APA),
		}))
	}
	if err := validateHPARoleSubtarget(pa, specPath); err != nil {
		allErrs = append(allErrs, err)
	}

	// 4. Validate MetricsSources
	metricsPath := specPath.Child("metricsSources")
	if len(pa.Spec.MetricsSources) != 1 {
		allErrs = append(allErrs, field.Invalid(metricsPath, pa.Spec.MetricsSources, "exactly one metricsSource is required"))
	} else {
		ms := &pa.Spec.MetricsSources[0]
		msPath := metricsPath.Index(0)

		if ms.TargetMetric == "" {
			allErrs = append(allErrs, field.Required(msPath.Child("targetMetric"), "must be set"))
		}
		if ms.TargetValue == "" {
			allErrs = append(allErrs, field.Required(msPath.Child("targetValue"), "must be set"))
		} else {
			qty, err := resource.ParseQuantity(ms.TargetValue)
			if err != nil {
				allErrs = append(allErrs, field.Invalid(msPath.Child("targetValue"), ms.TargetValue, "must be a valid number"))
			} else {
				if qty.Sign() <= 0 {
					allErrs = append(allErrs, field.Invalid(msPath.Child("targetValue"), ms.TargetValue, "must be greater than 0"))
				}
			}
		}

		switch ms.MetricSourceType {
		case autoscalingv1alpha1.POD:
			if ms.ProtocolType == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("protocolType"), "required for metricSourceType=pod"))
			}
			if ms.Port == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("port"), "required for metricSourceType=pod"))
			}
			if ms.Path == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("path"), "required for metricSourceType=pod"))
			}

		case autoscalingv1alpha1.EXTERNAL, autoscalingv1alpha1.DOMAIN:
			// Empty endpoint selects the Kubernetes external.metrics API instead of an HTTP metrics endpoint.
			if ms.Endpoint == "" {
				break
			}
			if ms.ProtocolType == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("protocolType"), "required for metricSourceType=external/domain"))
			}
			if ms.Endpoint == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("endpoint"), "required for metricSourceType=external/domain"))
			}
			if ms.Path == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("path"), "required for metricSourceType=external/domain"))
			}

		case autoscalingv1alpha1.RESOURCE:
			validMetrics := map[string]bool{"cpu": true, "memory": true}
			if !validMetrics[ms.TargetMetric] {
				allErrs = append(allErrs, field.NotSupported(msPath.Child("targetMetric"), ms.TargetMetric, []string{"cpu", "memory"}))
			}
			// Ensure no extra fields are set
			if ms.Port != "" {
				allErrs = append(allErrs, field.Forbidden(msPath.Child("port"), "not allowed for metricSourceType=resource"))
			}
			if ms.Endpoint != "" {
				allErrs = append(allErrs, field.Forbidden(msPath.Child("endpoint"), "not allowed for metricSourceType=resource"))
			}
			if ms.Path != "" {
				allErrs = append(allErrs, field.Forbidden(msPath.Child("path"), "not allowed for metricSourceType=resource"))
			}
			if ms.ProtocolType != "" {
				allErrs = append(allErrs, field.Forbidden(msPath.Child("protocolType"), "not allowed for metricSourceType=resource"))
			}

		case autoscalingv1alpha1.CUSTOM:
			// No required fields for custom metrics
			break

		default:
			allErrs = append(allErrs, field.NotSupported(msPath.Child("metricSourceType"), ms.MetricSourceType, []string{
				string(autoscalingv1alpha1.POD),
				string(autoscalingv1alpha1.EXTERNAL),
				string(autoscalingv1alpha1.DOMAIN),
				string(autoscalingv1alpha1.RESOURCE),
				string(autoscalingv1alpha1.CUSTOM),
			}))
		}
	}

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: autoscalingv1alpha1.GroupVersion.Group, Kind: "PodAutoscaler"},
		pa.Name,
		allErrs,
	)
}

func validateScheduledBounds(pa *autoscalingv1alpha1.PodAutoscaler, specPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	path := specPath.Child("scheduledBounds")
	names := make(map[string]int)
	for i, scheduled := range pa.Spec.ScheduledBounds {
		schedulePath := path.Index(i)
		if scheduled.Name == "" {
			errs = append(errs, field.Required(schedulePath.Child("name"), "must be specified"))
		} else if previous, exists := names[scheduled.Name]; exists {
			errs = append(errs, field.Duplicate(schedulePath.Child("name"), fmt.Sprintf("duplicates scheduledBounds[%d].name", previous)))
		} else {
			names[scheduled.Name] = i
		}

		if _, err := parseScheduledBoundsCron(scheduled.Cron); err != nil {
			errs = append(errs, field.Invalid(schedulePath.Child("cron"), scheduled.Cron, err.Error()))
		}
		if scheduled.Duration.Duration <= 0 {
			errs = append(errs, field.Invalid(schedulePath.Child("duration"), scheduled.Duration.Duration.String(), "must be positive"))
		}
		if _, err := scheduledBoundsLocation(scheduled.Timezone); err != nil {
			errs = append(errs, field.Invalid(schedulePath.Child("timezone"), scheduled.Timezone, err.Error()))
		}
		if scheduled.StartTime != nil && scheduled.EndTime != nil && !scheduled.StartTime.Before(scheduled.EndTime) {
			errs = append(errs, field.Invalid(schedulePath, scheduled, "startTime must be earlier than endTime"))
		}
		if scheduled.MinReplicas == nil && scheduled.MaxReplicas == nil {
			errs = append(errs, field.Required(schedulePath, "at least one of minReplicas or maxReplicas must be specified"))
			continue
		}

		minReplicas := int32(1)
		if pa.Spec.MinReplicas != nil {
			minReplicas = *pa.Spec.MinReplicas
		}
		if scheduled.MinReplicas != nil {
			minReplicas = *scheduled.MinReplicas
		}
		maxReplicas := pa.Spec.MaxReplicas
		if scheduled.MaxReplicas != nil {
			maxReplicas = *scheduled.MaxReplicas
		}
		if minReplicas < 0 {
			errs = append(errs, field.Invalid(schedulePath.Child("minReplicas"), minReplicas, "effective minReplicas must not be negative"))
		}
		if maxReplicas <= 0 {
			errs = append(errs, field.Invalid(schedulePath.Child("maxReplicas"), maxReplicas, "effective maxReplicas must be positive"))
		}
		if minReplicas > maxReplicas {
			errs = append(errs, field.Invalid(schedulePath, scheduled, "effective minReplicas must not be greater than effective maxReplicas"))
		}
	}

	for i := range pa.Spec.ScheduledBounds {
		for j := 0; j < i; j++ {
			if scheduledBoundsOverlap(pa.Spec.ScheduledBounds[j], pa.Spec.ScheduledBounds[i]) {
				errs = append(errs, field.Invalid(path.Index(i), pa.Spec.ScheduledBounds[i].Name, "overlaps another scheduled bounds window"))
			}
		}
	}

	return errs
}

func scheduledBoundsLocation(timezone string) (*time.Location, error) {
	if timezone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(timezone)
}

func scheduledBoundsOverlap(left, right autoscalingv1alpha1.ScheduledReplicaBounds) bool {
	if !scheduledBoundsLifetimesOverlap(left.StartTime, left.EndTime, right.StartTime, right.EndTime) {
		return false
	}

	leftCron, err := parseScheduledBoundsCron(left.Cron)
	if err != nil {
		return false
	}
	rightCron, err := parseScheduledBoundsCron(right.Cron)
	if err != nil {
		return false
	}
	leftLocation, err := scheduledBoundsLocation(left.Timezone)
	if err != nil {
		return false
	}
	rightLocation, err := scheduledBoundsLocation(right.Timezone)
	if err != nil {
		return false
	}

	weekStart, weekEnd := scheduledBoundsOverlapWeek(left, right)
	leftWindows := leftCron.windowsOverlappingWeek(left, leftLocation, weekStart, weekEnd)
	rightWindows := rightCron.windowsOverlappingWeek(right, rightLocation, weekStart, weekEnd)
	for _, leftWindow := range leftWindows {
		for _, rightWindow := range rightWindows {
			if leftWindow.start.Before(rightWindow.end) && rightWindow.start.Before(leftWindow.end) {
				return true
			}
		}
	}
	return false
}

func scheduledBoundsOverlapWeek(left, right autoscalingv1alpha1.ScheduledReplicaBounds) (time.Time, time.Time) {
	anchor := time.Date(2000, time.January, 3, 0, 0, 0, 0, time.UTC)
	hasStart := false
	if left.StartTime != nil && left.StartTime.After(anchor) {
		anchor = left.StartTime.Time
		hasStart = true
	}
	if right.StartTime != nil && right.StartTime.After(anchor) {
		anchor = right.StartTime.Time
		hasStart = true
	}
	if !hasStart {
		hasEnd := false
		if left.EndTime != nil {
			anchor = left.EndTime.Time.AddDate(0, 0, -scheduledBoundsValidationWeekDays)
			hasEnd = true
		}
		if right.EndTime != nil {
			candidate := right.EndTime.Time.AddDate(0, 0, -scheduledBoundsValidationWeekDays)
			if !hasEnd || candidate.Before(anchor) {
				anchor = candidate
			}
		}
	}

	dayStart := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, time.UTC)
	return dayStart, dayStart.AddDate(0, 0, scheduledBoundsValidationWeekDays)
}

func parseScheduledBoundsCron(expression string) (scheduledBoundsCron, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return scheduledBoundsCron{}, fmt.Errorf("unsupported cron syntax: expected five fields")
	}

	minute, err := parseScheduledBoundsNumber(fields[0], 0, 59)
	if err != nil {
		return scheduledBoundsCron{}, fmt.Errorf("unsupported cron syntax: minute must be a single number from 0 to 59")
	}
	hours, err := parseScheduledBoundsNumberSet(fields[1], 0, 23, nil)
	if err != nil {
		return scheduledBoundsCron{}, fmt.Errorf("unsupported cron syntax: hour must be numeric values or ranges from 0 to 23")
	}
	if fields[2] != "*" {
		return scheduledBoundsCron{}, fmt.Errorf("unsupported cron syntax: day-of-month must be *")
	}
	if fields[3] != "*" {
		return scheduledBoundsCron{}, fmt.Errorf("unsupported cron syntax: month must be *")
	}
	weekdays, err := parseScheduledBoundsNumberSet(fields[4], 0, 7, scheduledBoundsWeekdayNames)
	if err != nil {
		return scheduledBoundsCron{}, fmt.Errorf("unsupported cron syntax: day-of-week must be *, values, or ranges")
	}

	var schedule scheduledBoundsCron
	schedule.minute = minute
	for _, hour := range hours {
		schedule.hours[hour] = true
	}
	for _, weekday := range weekdays {
		schedule.weekdays[weekday%7] = true
	}
	return schedule, nil
}

var scheduledBoundsWeekdayNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

func parseScheduledBoundsNumberSet(value string, min, max int, names map[string]int) ([]int, error) {
	if value == "*" {
		if names == nil {
			return nil, fmt.Errorf("wildcards are not supported")
		}
		values := make([]int, 0, max-min+1)
		for value := min; value <= max; value++ {
			values = append(values, value)
		}
		return values, nil
	}

	values := make([]int, 0)
	for _, part := range strings.Split(value, ",") {
		if part == "" {
			return nil, fmt.Errorf("empty list value")
		}
		if strings.Count(part, "-") > 1 {
			return nil, fmt.Errorf("invalid range")
		}
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			start, err := parseScheduledBoundsValue(bounds[0], min, max, names)
			if err != nil {
				return nil, err
			}
			end, err := parseScheduledBoundsValue(bounds[1], min, max, names)
			if err != nil || start > end {
				return nil, fmt.Errorf("invalid range")
			}
			for value := start; value <= end; value++ {
				values = append(values, value)
			}
			continue
		}

		parsed, err := parseScheduledBoundsValue(part, min, max, names)
		if err != nil {
			return nil, err
		}
		values = append(values, parsed)
	}
	return values, nil
}

func parseScheduledBoundsValue(value string, min, max int, names map[string]int) (int, error) {
	if names != nil {
		if named, ok := names[strings.ToUpper(value)]; ok {
			return named, nil
		}
	}
	return parseScheduledBoundsNumber(value, min, max)
}

func parseScheduledBoundsNumber(value string, min, max int) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("empty number")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("not a number")
		}
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < min || number > max {
		return 0, fmt.Errorf("number out of range")
	}
	return number, nil
}

func (schedule scheduledBoundsCron) windowsOverlappingWeek(scheduled autoscalingv1alpha1.ScheduledReplicaBounds, location *time.Location, weekStart, weekEnd time.Time) []scheduledBoundsWindow {
	localStart := weekStart.In(location)
	dayStart := time.Date(localStart.Year(), localStart.Month(), localStart.Day(), 0, 0, 0, 0, location)
	windows := make([]scheduledBoundsWindow, 0)
	for dayOffset := -scheduledBoundsValidationWeekDays - 1; dayOffset <= scheduledBoundsValidationWeekDays+1; dayOffset++ {
		day := dayStart.AddDate(0, 0, dayOffset)
		if !schedule.weekdays[day.Weekday()] {
			continue
		}
		for hour, enabled := range schedule.hours {
			if !enabled {
				continue
			}
			start := time.Date(day.Year(), day.Month(), day.Day(), hour, schedule.minute, 0, 0, location).UTC()
			if start.In(location).Year() != day.Year() || start.In(location).Month() != day.Month() || start.In(location).Day() != day.Day() || start.In(location).Hour() != hour || start.In(location).Minute() != schedule.minute {
				continue
			}
			end := start.Add(scheduled.Duration.Duration)
			if scheduled.StartTime != nil && start.Before(scheduled.StartTime.Time) {
				start = scheduled.StartTime.Time
			}
			if scheduled.EndTime != nil && scheduled.EndTime.Time.Before(end) {
				end = scheduled.EndTime.Time
			}
			if start.Before(end) && start.Before(weekEnd) && weekStart.Before(end) {
				if start.Before(weekStart) {
					start = weekStart
				}
				if weekEnd.Before(end) {
					end = weekEnd
				}
				windows = append(windows, scheduledBoundsWindow{start: start, end: end})
			}
		}
	}
	return windows
}

func scheduledBoundsLifetimesOverlap(leftStart, leftEnd, rightStart, rightEnd *metav1.Time) bool {
	if leftEnd != nil && rightStart != nil && !rightStart.Time.Before(leftEnd.Time) {
		return false
	}
	if rightEnd != nil && leftStart != nil && !leftStart.Time.Before(rightEnd.Time) {
		return false
	}
	return true
}

func validateHPARoleSubtarget(pa *autoscalingv1alpha1.PodAutoscaler, specPath *field.Path) *field.Error {
	if pa.Spec.ScalingStrategy != autoscalingv1alpha1.HPA ||
		pa.Spec.SubTargetSelector == nil ||
		pa.Spec.SubTargetSelector.RoleName == "" {
		return nil
	}

	return field.Forbidden(
		specPath.Child("subTargetSelector").Child("roleName"),
		"not supported with scalingStrategy=HPA; use APA or KPA for StormService role-level autoscaling",
	)
}

func validateMetricWindows(pa *autoscalingv1alpha1.PodAutoscaler, specPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	observeWindow := defaultObserveWindowSeconds
	if pa.Spec.ObserveWindowSeconds != nil {
		observeWindow = *pa.Spec.ObserveWindowSeconds
		if observeWindow <= 0 {
			allErrs = append(allErrs, field.Invalid(specPath.Child("observeWindowSeconds"), observeWindow, "must be greater than 0"))
		}
		if observeWindow > maxMetricWindowSeconds {
			allErrs = append(allErrs, field.Invalid(specPath.Child("observeWindowSeconds"), observeWindow, fmt.Sprintf("must be less than or equal to %d", maxMetricWindowSeconds)))
		}
	}

	panicWindow := defaultPanicWindowSeconds
	if pa.Spec.PanicWindowSeconds != nil {
		panicWindow = *pa.Spec.PanicWindowSeconds
		if panicWindow <= 0 {
			allErrs = append(allErrs, field.Invalid(specPath.Child("panicWindowSeconds"), panicWindow, "must be greater than 0"))
		}
		if panicWindow > maxMetricWindowSeconds {
			allErrs = append(allErrs, field.Invalid(specPath.Child("panicWindowSeconds"), panicWindow, fmt.Sprintf("must be less than or equal to %d", maxMetricWindowSeconds)))
		}
	}
	if panicWindow > observeWindow {
		allErrs = append(allErrs, field.Invalid(specPath.Child("panicWindowSeconds"), panicWindow, "must be less than or equal to observeWindowSeconds"))
	}

	return allErrs
}
