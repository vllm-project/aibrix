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

package handler

import (
	"context"
	"net/http"
	"sort"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/apps/console/api/error_injection"
	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
)

// InjectionHandler implements console.v1.InjectionService.
type InjectionHandler struct {
	pb.UnimplementedInjectionServiceServer

	injector error_injection.Injector
	planner  plannerapi.Planner
}

// NewInjectionHandler creates a new InjectionHandler.
func NewInjectionHandler(injector error_injection.Injector, planner plannerapi.Planner) *InjectionHandler {
	return &InjectionHandler{injector: injector, planner: planner}
}

// ListInjectionPoints returns all registered injection points.
func (h *InjectionHandler) ListInjectionPoints(ctx context.Context, req *pb.ListInjectionPointsRequest) (*pb.ListInjectionPointsResponse, error) {
	// Get all injection points from the default registry
	points := error_injection.GetDefaultRegistry()

	// Convert to proto format
	pbPoints := make([]*pb.InjectionPoint, 0, len(points))
	for _, point := range points {
		pbPoints = append(pbPoints, convertInjectionPointToPB(point))
	}

	// Sort the points by ID to ensure a deterministic API response order
	sort.Slice(pbPoints, func(i, j int) bool {
		return pbPoints[i].Id < pbPoints[j].Id
	})

	return &pb.ListInjectionPointsResponse{Points: pbPoints}, nil
}

// GetInjectionConfig returns the current global injection configuration.
func (h *InjectionHandler) GetInjectionConfig(ctx context.Context, req *pb.GetInjectionConfigRequest) (*pb.GetInjectionConfigResponse, error) {
	config := h.injector.GetGlobalConfig()
	if config == nil {
		return &pb.GetInjectionConfigResponse{Config: nil}, nil
	}

	return &pb.GetInjectionConfigResponse{
		Config: convertGlobalConfigToPB(config),
	}, nil
}

// SetInjectionConfig updates the global injection configuration.
func (h *InjectionHandler) SetInjectionConfig(ctx context.Context, req *pb.SetInjectionConfigRequest) (*pb.SetInjectionConfigResponse, error) {
	if req.Config == nil {
		return nil, status.Error(codes.InvalidArgument, "config is required")
	}

	config := convertPBToGlobalConfig(req.Config)
	if err := h.injector.SetGlobalConfig(config); err != nil {
		klog.Errorf("Failed to set injection config: %v", err)
		return nil, status.Errorf(codes.Internal, "failed to set config: %v", err)
	}

	klog.Infof("Updated global injection config: enabled=%v, rules=%d", config.Enabled, len(config.Rules))

	return &pb.SetInjectionConfigResponse{
		Config: convertGlobalConfigToPB(config),
	}, nil
}

// ClearInjectionConfig clears the global injection configuration (disables injection).
func (h *InjectionHandler) ClearInjectionConfig(ctx context.Context, req *pb.ClearInjectionConfigRequest) (*emptypb.Empty, error) {
	emptyConfig := &error_injection.GlobalInjectionConfig{
		Enabled:        false,
		Rules:          []error_injection.InjectionRule{},
		ExcludedPoints: []string{},
	}

	if err := h.injector.SetGlobalConfig(emptyConfig); err != nil {
		klog.Errorf("Failed to clear injection config: %v", err)
		return nil, status.Errorf(codes.Internal, "failed to clear config: %v", err)
	}

	klog.Info("Cleared global injection config")
	return &emptypb.Empty{}, nil
}

// GetInjectionTrace retrieves the execution trace for a specific job.
func (h *InjectionHandler) GetInjectionTrace(ctx context.Context, req *pb.GetInjectionTraceRequest) (*pb.GetInjectionTraceResponse, error) {
	if req.JobId == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}

	trace := h.injector.GetTrace(ctx, req.JobId)
	if trace == nil || trace.JobID != req.JobId {
		return nil, status.Error(codes.NotFound, "trace not found")
	}

	return &pb.GetInjectionTraceResponse{
		Trace: convertTraceToPB(trace),
	}, nil
}

// Conversion functions between internal types and proto types

func convertInjectionPointToPB(point *error_injection.InjectionPoint) *pb.InjectionPoint {
	if point == nil {
		return nil
	}

	templates := make(map[string]*pb.InjectionTemplate)
	for et, tpl := range point.Templates {
		templates[string(et)] = convertTemplateToPB(tpl)
	}

	return &pb.InjectionPoint{
		Id:          point.ID,
		Component:   point.Component,
		Action:      point.Action,
		Description: point.Description,
		Templates:   templates,
	}
}

func convertTemplateToPB(tpl *error_injection.InjectionTemplate) *pb.InjectionTemplate {
	if tpl == nil {
		return nil
	}

	return &pb.InjectionTemplate{
		Type:            convertErrorTypeToPB(tpl.Type),
		Code:            tpl.Code,
		MessageTemplate: tpl.MessageTemplate,
		Placeholders:    tpl.Placeholders,
		DetailsTemplate: tpl.DetailsTemplate,
	}
}

func convertErrorTypeToPB(et error_injection.ErrorType) pb.ErrorType {
	switch et {
	case error_injection.ErrorTypeTimeout:
		return pb.ErrorType_ERROR_TYPE_TIMEOUT
	case error_injection.ErrorTypeUnavailable:
		return pb.ErrorType_ERROR_TYPE_UNAVAILABLE
	case error_injection.ErrorTypeInvalidArgument:
		return pb.ErrorType_ERROR_TYPE_INVALID_ARGUMENT
	case error_injection.ErrorTypeNotFound:
		return pb.ErrorType_ERROR_TYPE_NOT_FOUND
	case error_injection.ErrorTypePermissionDenied:
		return pb.ErrorType_ERROR_TYPE_PERMISSION_DENIED
	case error_injection.ErrorTypeResourceExhausted:
		return pb.ErrorType_ERROR_TYPE_RESOURCE_EXHAUSTED
	case error_injection.ErrorTypeInternal:
		return pb.ErrorType_ERROR_TYPE_INTERNAL
	case error_injection.ErrorTypeCrash:
		return pb.ErrorType_ERROR_TYPE_CRASH
	default:
		return pb.ErrorType_ERROR_TYPE_UNSPECIFIED
	}
}

func convertGlobalConfigToPB(config *error_injection.GlobalInjectionConfig) *pb.GlobalInjectionConfig {
	if config == nil {
		return nil
	}

	rules := make([]*pb.InjectionRule, 0, len(config.Rules))
	for _, rule := range config.Rules {
		rules = append(rules, convertRuleToPB(rule))
	}

	return &pb.GlobalInjectionConfig{
		Enabled:           config.Enabled,
		Rules:             rules,
		ExcludedPoints:    config.ExcludedPoints,
		GlobalProbability: config.GlobalProbability,
		PointWeights:      config.PointWeights,
	}
}

func convertRuleToPB(rule error_injection.InjectionRule) *pb.InjectionRule {
	return &pb.InjectionRule{
		PointRef:    rule.PointRef,
		ErrorType:   convertErrorTypeToPB(rule.ErrorType),
		Probability: rule.Probability,
		Overrides:   rule.Overrides,
	}
}

func convertPBToGlobalConfig(pbConfig *pb.GlobalInjectionConfig) *error_injection.GlobalInjectionConfig {
	if pbConfig == nil {
		return nil
	}

	rules := make([]error_injection.InjectionRule, 0, len(pbConfig.Rules))
	for _, pbRule := range pbConfig.Rules {
		rules = append(rules, convertPBToRule(pbRule))
	}

	return &error_injection.GlobalInjectionConfig{
		Enabled:           pbConfig.Enabled,
		Rules:             rules,
		ExcludedPoints:    pbConfig.ExcludedPoints,
		GlobalProbability: pbConfig.GlobalProbability,
		PointWeights:      pbConfig.PointWeights,
	}
}

func convertPBToRule(pbRule *pb.InjectionRule) error_injection.InjectionRule {
	return error_injection.InjectionRule{
		PointRef:    pbRule.PointRef,
		ErrorType:   convertPBToErrorType(pbRule.ErrorType),
		Probability: pbRule.Probability,
		Overrides:   pbRule.Overrides,
	}
}

func convertPBToInjectionConfig(pbConfig *pb.InjectionConfig) *error_injection.InjectionConfig {
	if pbConfig == nil {
		return nil
	}

	rules := make([]error_injection.InjectionRule, 0, len(pbConfig.Rules))
	for _, pbRule := range pbConfig.Rules {
		rules = append(rules, convertPBToRule(pbRule))
	}

	return &error_injection.InjectionConfig{
		Enabled:           pbConfig.Enabled,
		Rules:             rules,
		GlobalProbability: pbConfig.GlobalProbability,
	}
}

func convertPBToErrorType(pbET pb.ErrorType) error_injection.ErrorType {
	switch pbET {
	case pb.ErrorType_ERROR_TYPE_TIMEOUT:
		return error_injection.ErrorTypeTimeout
	case pb.ErrorType_ERROR_TYPE_UNAVAILABLE:
		return error_injection.ErrorTypeUnavailable
	case pb.ErrorType_ERROR_TYPE_INVALID_ARGUMENT:
		return error_injection.ErrorTypeInvalidArgument
	case pb.ErrorType_ERROR_TYPE_NOT_FOUND:
		return error_injection.ErrorTypeNotFound
	case pb.ErrorType_ERROR_TYPE_PERMISSION_DENIED:
		return error_injection.ErrorTypePermissionDenied
	case pb.ErrorType_ERROR_TYPE_RESOURCE_EXHAUSTED:
		return error_injection.ErrorTypeResourceExhausted
	case pb.ErrorType_ERROR_TYPE_INTERNAL:
		return error_injection.ErrorTypeInternal
	case pb.ErrorType_ERROR_TYPE_CRASH:
		return error_injection.ErrorTypeCrash
	default:
		return error_injection.ErrorType("")
	}
}

func convertTraceToPB(trace *error_injection.ExecutionTrace) *pb.ExecutionTrace {
	if trace == nil {
		return nil
	}

	points := make([]*pb.PointRecord, 0, len(trace.Points))
	for _, point := range trace.Points {
		points = append(points, convertPointRecordToPB(point))
	}

	return &pb.ExecutionTrace{
		JobId:     trace.JobID,
		StartTime: trace.StartTime.Unix(),
		EndTime:   trace.EndTime.Unix(),
		Points:    points,
	}
}

func convertPointRecordToPB(record error_injection.PointRecord) *pb.PointRecord {
	return &pb.PointRecord{
		PointId:          record.PointID,
		Timestamp:        record.Timestamp.Unix(),
		Triggered:        record.Triggered,
		ContextSnapshot:  record.ContextSnapshot,
		Error:            convertInjectedErrorToPB(record.Error),
		TemplateUsed:     convertErrorTypeToPB(record.TemplateUsed),
		OverridesApplied: record.OverridesApplied,
		ProbabilityRoll:  record.ProbabilityRoll,
	}
}

func convertInjectedErrorToPB(err *error_injection.InjectedError) *pb.InjectedError {
	if err == nil {
		return nil
	}

	return &pb.InjectedError{
		Type:    convertErrorTypeToPB(err.Type),
		Code:    err.Code,
		Message: err.Message,
		Details: err.Details,
	}
}

func convertInjectedErrorToHttpCode(err error) int {
	statusCode := http.StatusInternalServerError
	if injErr, ok := err.(*error_injection.InjectedError); ok {
		switch injErr.Type {
		case error_injection.ErrorTypeInvalidArgument:
			statusCode = http.StatusBadRequest
		case error_injection.ErrorTypePermissionDenied:
			statusCode = http.StatusForbidden
		case error_injection.ErrorTypeNotFound:
			statusCode = http.StatusNotFound
		case error_injection.ErrorTypeUnavailable, error_injection.ErrorTypeResourceExhausted:
			statusCode = http.StatusServiceUnavailable
		case error_injection.ErrorTypeTimeout:
			statusCode = http.StatusRequestTimeout
		case error_injection.ErrorTypeInternal, error_injection.ErrorTypeCrash:
			statusCode = http.StatusInternalServerError
		}
	}
	return statusCode
}
