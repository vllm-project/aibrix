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

// Injection point constants for the console.
const (
	POINT_CONSOLE_CREATE_JOB    = "console.create_job"
	POINT_CONSOLE_GET_JOB       = "console.get_job"
	POINT_CONSOLE_CANCEL_JOB    = "console.cancel_job"
	POINT_CONSOLE_LIST_JOBS     = "console.list_jobs"
	POINT_CONSOLE_UPLOAD_FILE   = "console.upload_file"
	POINT_CONSOLE_DOWNLOAD_FILE = "console.download_file"
)

// Injection point constants for the planner.
const (
	POINT_PLANNER_ENQUEUE      = "planner.enqueue"
	POINT_PLANNER_PLAN         = "planner.plan"
	POINT_PLANNER_SUBMIT_BATCH = "planner.submit_batch"
	POINT_PLANNER_CANCEL       = "planner.cancel"
	POINT_PLANNER_PERSIST      = "planner.persist"
	POINT_PLANNER_RECOVER      = "planner.recover"
)

// Injection point constants for the resource manager.
const (
	POINT_RM_PROVISION      = "rm.provision"
	POINT_RM_GET_STATUS     = "rm.get_status"
	POINT_RM_RELEASE        = "rm.release"
	POINT_RM_CATALOG_LOOKUP = "rm.catalog_lookup"
	POINT_RM_LIST           = "rm.list"
)

// Injection point constants for the batch client.
const (
	POINT_BATCH_CLIENT_CREATE_BATCH = "batch_client.create_batch"
	POINT_BATCH_CLIENT_GET_BATCH    = "batch_client.get_batch"
	POINT_BATCH_CLIENT_CANCEL_BATCH = "batch_client.cancel_batch"
	POINT_BATCH_CLIENT_LIST_BATCHES = "batch_client.list_batches"
)

// Injection point constants for the store.
const (
	POINT_STORE_UPSERT_JOB       = "store.upsert_job"
	POINT_STORE_GET_JOB          = "store.get_job"
	POINT_STORE_LIST_JOBS        = "store.list_jobs"
	POINT_STORE_UPSERT_PROVISION = "store.upsert_provision"
	POINT_STORE_GET_PROVISION    = "store.get_provision"
)
