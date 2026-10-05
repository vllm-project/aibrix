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

package error_injection

// DefaultRegistry holds all pre-defined injection points
var DefaultRegistry map[string]*InjectionPoint

// init populates the registry with all injection points
func init() {
	DefaultRegistry = make(map[string]*InjectionPoint)

	// Console Layer injection points
	registerConsoleInjectionPoints()

	// Planner injection points
	registerPlannerInjectionPoints()

	// Resource Manager injection points
	registerResourceManagerInjectionPoints()

	// Batch Client injection points
	registerBatchClientInjectionPoints()

	// Store injection points
	registerStoreInjectionPoints()
}

// registerConsoleInjectionPoints registers all console layer injection points
func registerConsoleInjectionPoints() {
	// console.create_job
	DefaultRegistry[POINT_CONSOLE_CREATE_JOB] = &InjectionPoint{
		ID:          POINT_CONSOLE_CREATE_JOB,
		Component:   "console",
		Action:      "create_job",
		Description: "Job creation handler",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeInvalidArgument: {
				Type:            ErrorTypeInvalidArgument,
				Code:            CodeInvalidArgument,
				MessageTemplate: "invalid job spec: {{.reason}}",
				Placeholders:    map[string]string{"reason": "missing required field"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "job service unavailable: {{.service}}",
				Placeholders:    map[string]string{"service": "job_handler"},
			},
			ErrorTypePermissionDenied: {
				Type:            ErrorTypePermissionDenied,
				Code:            CodePermissionDenied,
				MessageTemplate: "permission denied: {{.action}} on {{.resource}}",
				Placeholders:    map[string]string{"action": "create", "resource": "job"},
			},
		},
	}

	// console.get_job
	DefaultRegistry[POINT_CONSOLE_GET_JOB] = &InjectionPoint{
		ID:          POINT_CONSOLE_GET_JOB,
		Component:   "console",
		Action:      "get_job",
		Description: "Job retrieval handler",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeNotFound: {
				Type:            ErrorTypeNotFound,
				Code:            CodeNotFound,
				MessageTemplate: "job {{.job_id}} not found",
				Placeholders:    map[string]string{"job_id": "test-job-123"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "job service unavailable: {{.service}}",
				Placeholders:    map[string]string{"service": "job_handler"},
			},
		},
	}

	// console.cancel_job
	DefaultRegistry[POINT_CONSOLE_CANCEL_JOB] = &InjectionPoint{
		ID:          POINT_CONSOLE_CANCEL_JOB,
		Component:   "console",
		Action:      "cancel_job",
		Description: "Job cancellation handler",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeNotFound: {
				Type:            ErrorTypeNotFound,
				Code:            CodeNotFound,
				MessageTemplate: "job {{.job_id}} not found",
				Placeholders:    map[string]string{"job_id": "test-job-123"},
			},
			ErrorTypePermissionDenied: {
				Type:            ErrorTypePermissionDenied,
				Code:            CodePermissionDenied,
				MessageTemplate: "permission denied: {{.action}} on {{.resource}}",
				Placeholders:    map[string]string{"action": "cancel", "resource": "job"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "job service unavailable: {{.service}}",
				Placeholders:    map[string]string{"service": "job_handler"},
			},
		},
	}

	// console.list_jobs
	DefaultRegistry[POINT_CONSOLE_LIST_JOBS] = &InjectionPoint{
		ID:          POINT_CONSOLE_LIST_JOBS,
		Component:   "console",
		Action:      "list_jobs",
		Description: "Job listing handler",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "job service unavailable: {{.service}}",
				Placeholders:    map[string]string{"service": "job_handler"},
			},
		},
	}

	// console.upload_file
	DefaultRegistry[POINT_CONSOLE_UPLOAD_FILE] = &InjectionPoint{
		ID:          POINT_CONSOLE_UPLOAD_FILE,
		Component:   "console",
		Action:      "upload_file",
		Description: "File upload handler",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeInvalidArgument: {
				Type:            ErrorTypeInvalidArgument,
				Code:            CodeInvalidArgument,
				MessageTemplate: "invalid file: {{.reason}}",
				Placeholders:    map[string]string{"reason": "file size exceeds limit"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "storage service unavailable: {{.service}}",
				Placeholders:    map[string]string{"service": "file_storage"},
			},
		},
	}

	// console.download_file
	DefaultRegistry[POINT_CONSOLE_DOWNLOAD_FILE] = &InjectionPoint{
		ID:          POINT_CONSOLE_DOWNLOAD_FILE,
		Component:   "console",
		Action:      "download_file",
		Description: "File download handler",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeNotFound: {
				Type:            ErrorTypeNotFound,
				Code:            CodeNotFound,
				MessageTemplate: "file {{.file_id}} not found",
				Placeholders:    map[string]string{"file_id": "test-file-123"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "storage service unavailable: {{.service}}",
				Placeholders:    map[string]string{"service": "file_storage"},
			},
		},
	}
}

// registerPlannerInjectionPoints registers all planner injection points
func registerPlannerInjectionPoints() {
	// planner.enqueue
	DefaultRegistry[POINT_PLANNER_ENQUEUE] = &InjectionPoint{
		ID:          POINT_PLANNER_ENQUEUE,
		Component:   "planner",
		Action:      "enqueue",
		Description: "Job enqueue to planning queue",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeInvalidArgument: {
				Type:            ErrorTypeInvalidArgument,
				Code:            CodeInvalidArgument,
				MessageTemplate: "invalid job request: {{.reason}}",
				Placeholders:    map[string]string{"reason": "missing job spec"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "planner queue unavailable: {{.queue}}",
				Placeholders:    map[string]string{"queue": "planning_queue"},
			},
			ErrorTypeCrash: {
				Type:            ErrorTypeCrash,
				Code:            CodeCrash,
				MessageTemplate: "simulated crash at {{.point}}",
				Placeholders:    map[string]string{"point": "planner.enqueue"},
			},
		},
	}

	// planner.plan
	DefaultRegistry[POINT_PLANNER_PLAN] = &InjectionPoint{
		ID:          POINT_PLANNER_PLAN,
		Component:   "planner",
		Action:      "plan",
		Description: "Job planning logic",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeInternal: {
				Type:            ErrorTypeInternal,
				Code:            CodeInternal,
				MessageTemplate: "planning failed: {{.reason}}",
				Placeholders:    map[string]string{"reason": "internal planning error"},
			},
			ErrorTypeResourceExhausted: {
				Type:            ErrorTypeResourceExhausted,
				Code:            CodeResourceExhausted,
				MessageTemplate: "resource exhausted: {{.resource}}",
				Placeholders:    map[string]string{"resource": "gpu_memory"},
			},
		},
	}

	// planner.submit_batch
	DefaultRegistry[POINT_PLANNER_SUBMIT_BATCH] = &InjectionPoint{
		ID:          POINT_PLANNER_SUBMIT_BATCH,
		Component:   "planner",
		Action:      "submit_batch",
		Description: "Submit job to batch system",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "batch client unavailable: {{.client}}",
				Placeholders:    map[string]string{"client": "batch_client"},
			},
			ErrorTypeInvalidArgument: {
				Type:            ErrorTypeInvalidArgument,
				Code:            CodeInvalidArgument,
				MessageTemplate: "invalid batch request: {{.reason}}",
				Placeholders:    map[string]string{"reason": "invalid job spec"},
			},
		},
	}

	// planner.cancel
	DefaultRegistry[POINT_PLANNER_CANCEL] = &InjectionPoint{
		ID:          POINT_PLANNER_CANCEL,
		Component:   "planner",
		Action:      "cancel",
		Description: "Cancel planned job",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeNotFound: {
				Type:            ErrorTypeNotFound,
				Code:            CodeNotFound,
				MessageTemplate: "planned job {{.job_id}} not found",
				Placeholders:    map[string]string{"job_id": "test-job-123"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "planner unavailable: {{.component}}",
				Placeholders:    map[string]string{"component": "planner"},
			},
		},
	}

	// planner.persist
	DefaultRegistry[POINT_PLANNER_PERSIST] = &InjectionPoint{
		ID:          POINT_PLANNER_PERSIST,
		Component:   "planner",
		Action:      "persist",
		Description: "Persist job state to store",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "store unavailable: {{.store}}",
				Placeholders:    map[string]string{"store": "job_store"},
			},
		},
	}

	// planner.recover
	DefaultRegistry[POINT_PLANNER_RECOVER] = &InjectionPoint{
		ID:          POINT_PLANNER_RECOVER,
		Component:   "planner",
		Action:      "recover",
		Description: "Recover planner state",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "store unavailable during recovery: {{.store}}",
				Placeholders:    map[string]string{"store": "job_store"},
			},
			ErrorTypeInternal: {
				Type:            ErrorTypeInternal,
				Code:            CodeInternal,
				MessageTemplate: "recovery failed: {{.reason}}",
				Placeholders:    map[string]string{"reason": "inconsistent state"},
			},
			ErrorTypeCrash: {
				Type:            ErrorTypeCrash,
				Code:            CodeCrash,
				MessageTemplate: "simulated crash at {{.point}}",
				Placeholders:    map[string]string{"point": "planner.recover"},
			},
		},
	}
}

// registerResourceManagerInjectionPoints registers all resource manager injection points
func registerResourceManagerInjectionPoints() {
	// rm.provision
	DefaultRegistry[POINT_RM_PROVISION] = &InjectionPoint{
		ID:          POINT_RM_PROVISION,
		Component:   "rm",
		Action:      "provision",
		Description: "Resource provisioning",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "resource manager unavailable: {{.component}}",
				Placeholders:    map[string]string{"component": "provisioner"},
			},
			ErrorTypeResourceExhausted: {
				Type:            ErrorTypeResourceExhausted,
				Code:            CodeResourceExhausted,
				MessageTemplate: "no available resources: {{.resource_type}}",
				Placeholders:    map[string]string{"resource_type": "gpu"},
			},
			ErrorTypeTimeout: {
				Type:            ErrorTypeTimeout,
				Code:            CodeDeadlineExceeded,
				MessageTemplate: "provisioning timeout after {{.duration}}",
				Placeholders:    map[string]string{"duration": "10m"},
			},
			ErrorTypeCrash: {
				Type:            ErrorTypeCrash,
				Code:            CodeCrash,
				MessageTemplate: "simulated crash at {{.point}}",
				Placeholders:    map[string]string{"point": "rm.provision"},
			},
		},
	}

	// rm.get_status
	DefaultRegistry[POINT_RM_GET_STATUS] = &InjectionPoint{
		ID:          POINT_RM_GET_STATUS,
		Component:   "rm",
		Action:      "get_status",
		Description: "Get provisioning status",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeNotFound: {
				Type:            ErrorTypeNotFound,
				Code:            CodeNotFound,
				MessageTemplate: "provision {{.provision_id}} not found",
				Placeholders:    map[string]string{"provision_id": "prov-123"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "resource manager unavailable: {{.component}}",
				Placeholders:    map[string]string{"component": "status_checker"},
			},
		},
	}

	// rm.release
	DefaultRegistry[POINT_RM_RELEASE] = &InjectionPoint{
		ID:          POINT_RM_RELEASE,
		Component:   "rm",
		Action:      "release",
		Description: "Release provisioned resources",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "resource manager unavailable: {{.component}}",
				Placeholders:    map[string]string{"component": "releaser"},
			},
			ErrorTypeTimeout: {
				Type:            ErrorTypeTimeout,
				Code:            CodeDeadlineExceeded,
				MessageTemplate: "release timeout after {{.duration}}",
				Placeholders:    map[string]string{"duration": "30s"},
			},
			ErrorTypeCrash: {
				Type:            ErrorTypeCrash,
				Code:            CodeCrash,
				MessageTemplate: "simulated crash at {{.point}}",
				Placeholders:    map[string]string{"point": "rm.release"},
			},
		},
	}

	// rm.catalog_lookup
	DefaultRegistry[POINT_RM_CATALOG_LOOKUP] = &InjectionPoint{
		ID:          POINT_RM_CATALOG_LOOKUP,
		Component:   "rm",
		Action:      "catalog_lookup",
		Description: "Lookup resource in catalog",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeNotFound: {
				Type:            ErrorTypeNotFound,
				Code:            CodeNotFound,
				MessageTemplate: "resource {{.resource_name}} not found in catalog",
				Placeholders:    map[string]string{"resource_name": "gpu-a100"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "catalog unavailable: {{.catalog}}",
				Placeholders:    map[string]string{"catalog": "resource_catalog"},
			},
		},
	}
}

// registerBatchClientInjectionPoints registers all batch client injection points
func registerBatchClientInjectionPoints() {
	// batch_client.create_batch
	DefaultRegistry[POINT_BATCH_CLIENT_CREATE_BATCH] = &InjectionPoint{
		ID:          POINT_BATCH_CLIENT_CREATE_BATCH,
		Component:   "batch_client",
		Action:      "create_batch",
		Description: "Create batch job",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "batch service unavailable: {{.service}}",
				Placeholders:    map[string]string{"service": "batch_api"},
			},
			ErrorTypeInvalidArgument: {
				Type:            ErrorTypeInvalidArgument,
				Code:            CodeInvalidArgument,
				MessageTemplate: "invalid batch request: {{.reason}}",
				Placeholders:    map[string]string{"reason": "invalid spec"},
			},
			ErrorTypeTimeout: {
				Type:            ErrorTypeTimeout,
				Code:            CodeDeadlineExceeded,
				MessageTemplate: "batch creation timeout after {{.duration}}",
				Placeholders:    map[string]string{"duration": "60s"},
			},
		},
	}

	// batch_client.get_batch
	DefaultRegistry[POINT_BATCH_CLIENT_GET_BATCH] = &InjectionPoint{
		ID:          POINT_BATCH_CLIENT_GET_BATCH,
		Component:   "batch_client",
		Action:      "get_batch",
		Description: "Get batch job status",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeNotFound: {
				Type:            ErrorTypeNotFound,
				Code:            CodeNotFound,
				MessageTemplate: "batch {{.batch_id}} not found",
				Placeholders:    map[string]string{"batch_id": "batch-123"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "batch service unavailable: {{.service}}",
				Placeholders:    map[string]string{"service": "batch_api"},
			},
		},
	}

	// batch_client.cancel_batch
	DefaultRegistry[POINT_BATCH_CLIENT_CANCEL_BATCH] = &InjectionPoint{
		ID:          POINT_BATCH_CLIENT_CANCEL_BATCH,
		Component:   "batch_client",
		Action:      "cancel_batch",
		Description: "Cancel batch job",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeNotFound: {
				Type:            ErrorTypeNotFound,
				Code:            CodeNotFound,
				MessageTemplate: "batch {{.batch_id}} not found",
				Placeholders:    map[string]string{"batch_id": "batch-123"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "batch service unavailable: {{.service}}",
				Placeholders:    map[string]string{"service": "batch_api"},
			},
		},
	}

	// batch_client.list_batches
	DefaultRegistry[POINT_BATCH_CLIENT_LIST_BATCHES] = &InjectionPoint{
		ID:          POINT_BATCH_CLIENT_LIST_BATCHES,
		Component:   "batch_client",
		Action:      "list_batches",
		Description: "List batch jobs",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "batch service unavailable: {{.service}}",
				Placeholders:    map[string]string{"service": "batch_api"},
			},
		},
	}
}

// registerStoreInjectionPoints registers all store injection points
func registerStoreInjectionPoints() {
	// store.upsert_job
	DefaultRegistry[POINT_STORE_UPSERT_JOB] = &InjectionPoint{
		ID:          POINT_STORE_UPSERT_JOB,
		Component:   "store",
		Action:      "upsert_job",
		Description: "Upsert job to store",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "store unavailable: {{.store}}",
				Placeholders:    map[string]string{"store": "job_store"},
			},
			ErrorTypeInternal: {
				Type:            ErrorTypeInternal,
				Code:            CodeInternal,
				MessageTemplate: "upsert failed: {{.reason}}",
				Placeholders:    map[string]string{"reason": "database error"},
			},
			ErrorTypeCrash: {
				Type:            ErrorTypeCrash,
				Code:            CodeCrash,
				MessageTemplate: "simulated crash at {{.point}}",
				Placeholders:    map[string]string{"point": "store.upsert_job"},
			},
		},
	}

	// store.get_job
	DefaultRegistry[POINT_STORE_GET_JOB] = &InjectionPoint{
		ID:          POINT_STORE_GET_JOB,
		Component:   "store",
		Action:      "get_job",
		Description: "Get job from store",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeNotFound: {
				Type:            ErrorTypeNotFound,
				Code:            CodeNotFound,
				MessageTemplate: "job {{.job_id}} not found",
				Placeholders:    map[string]string{"job_id": "test-job-123"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "store unavailable: {{.store}}",
				Placeholders:    map[string]string{"store": "job_store"},
			},
		},
	}

	// store.list_jobs
	DefaultRegistry[POINT_STORE_LIST_JOBS] = &InjectionPoint{
		ID:          POINT_STORE_LIST_JOBS,
		Component:   "store",
		Action:      "list_jobs",
		Description: "List jobs from store",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "store unavailable: {{.store}}",
				Placeholders:    map[string]string{"store": "job_store"},
			},
		},
	}

	// store.upsert_provision
	DefaultRegistry[POINT_STORE_UPSERT_PROVISION] = &InjectionPoint{
		ID:          POINT_STORE_UPSERT_PROVISION,
		Component:   "store",
		Action:      "upsert_provision",
		Description: "Upsert provision to store",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "store unavailable: {{.store}}",
				Placeholders:    map[string]string{"store": "provision_store"},
			},
			ErrorTypeCrash: {
				Type:            ErrorTypeCrash,
				Code:            CodeCrash,
				MessageTemplate: "simulated crash at {{.point}}",
				Placeholders:    map[string]string{"point": "store.upsert_provision"},
			},
		},
	}

	// store.get_provision
	DefaultRegistry[POINT_STORE_GET_PROVISION] = &InjectionPoint{
		ID:          POINT_STORE_GET_PROVISION,
		Component:   "store",
		Action:      "get_provision",
		Description: "Get provision from store",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeNotFound: {
				Type:            ErrorTypeNotFound,
				Code:            CodeNotFound,
				MessageTemplate: "provision {{.provision_id}} not found",
				Placeholders:    map[string]string{"provision_id": "prov-123"},
			},
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "store unavailable: {{.store}}",
				Placeholders:    map[string]string{"store": "provision_store"},
			},
		},
	}
}

// GetDefaultRegistry returns the default registry containing all pre-defined injection points
func GetDefaultRegistry() map[string]*InjectionPoint {
	return DefaultRegistry
}

// ValidatePointID checks if the given point ID exists in the default registry
func ValidatePointID(pointID string) bool {
	_, exists := DefaultRegistry[pointID]
	return exists
}

// GetInjectionPoint retrieves an injection point by ID from the default registry
func GetInjectionPoint(pointID string) (*InjectionPoint, bool) {
	point, exists := DefaultRegistry[pointID]
	return point, exists
}

// ListInjectionPoints returns all available injection point IDs
func ListInjectionPoints() []string {
	points := make([]string, 0, len(DefaultRegistry))
	for id := range DefaultRegistry {
		points = append(points, id)
	}
	return points
}

// GetInjectionTemplate retrieves a specific injection template from an injection point
func GetInjectionTemplate(pointID string, errorType ErrorType) (*InjectionTemplate, bool) {
	point, exists := DefaultRegistry[pointID]
	if !exists {
		return nil, false
	}

	template, exists := point.Templates[errorType]
	return template, exists
}
