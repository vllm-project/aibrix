# Error Injection End-to-End Examples Guide

This document provides complete end-to-end examples covering the entire workflow from job creation (with error injection) to trace querying.

---

## Example 1: Testing Resource Provisioning Timeout

### Scenario Description

Test whether the system can correctly handle resource provisioning timeouts and return meaningful error messages.

### Step 1: Create a Job with Injection Config

```bash
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "input_dataset": "file-test-timeout-001",
    "endpoint": "/v1/chat/completions",
    "name": "Provisioning Timeout Test Job",
    "injection_config": {
      "enabled": true,
      "rules": [
        {
          "point_ref": "rm.provision",
          "error_type": "ERROR_TYPE_TIMEOUT",
          "probability": 1.0,
          "overrides": {
            "duration": "3m",
            "resource_type": "A100 GPU"
          }
        }
      ]
    }
  }'
```

**Expected Response:**

```json
{
  "id": "job_739de962-f830-47cc-b4db-8e9e7631349d",
  "object": "batch",
  "endpoint": "/v1/chat/completions",
  "model": "",
  "inputDataset": "file-test-timeout-001",
  "completionWindow": "24h",
  "status": "queued",
  "outputDataset": "",
  "errorDataset": "",
  "createdAt": "1781320519",
  "inProgressAt": "0",
  "expiresAt": "0",
  "finalizingAt": "0",
  "completedAt": "0",
  "failedAt": "0",
  "expiredAt": "0",
  "cancellingAt": "0",
  "cancelledAt": "0",
  "requestCounts": null,
  "usage": null,
  "metadata": {
    "aibrix.console.created_by": "test@aibrix.ai",
    "aibrix.console.display_name": "Provisioning Timeout Test Job",
    "display_name": "Provisioning Timeout Test Job"
  },
  "extraBody": {},
  "errors": [],
  "runtime": null,
  "resourceAllocation": null,
  "modelTemplateRef": null,
  "profile": null,
  "name": "Provisioning Timeout Test Job",
  "createdBy": "test@aibrix.ai",
  "modelTemplateName": "",
  "modelTemplateVersion": "",
  "batchId": "",
  "provisionId": "",
  "errorMessage": "",
  "queuedAt": "1781320519",
  "resourcePreparingAt": "0",
  "submittingAt": "0",
  "resourceFailedAt": "0",
  "submitFailedAt": "0",
  "cancelRequestedAt": "0",
  "provision": null,
  "events": [
    {
      "id": "queued",
      "label": "Queued",
      "status": "queued",
      "source": "planner",
      "at": "1781320519",
      "message": "Console accepted the job."
    }
  ]
}
```

### Step 2: Wait for Job Processing

```bash
# Wait a few seconds and query job status
sleep 5

# Replace the job ID with the actual job ID
curl http://localhost:8080/api/v1/jobs/job_739de962-f830-47cc-b4db-8e9e7631349d
```

**Expected Response (Failed Status):**

```json
{
  "id": "job_739de962-f830-47cc-b4db-8e9e7631349d",
  "object": "batch",
  "endpoint": "/v1/chat/completions",
  "model": "",
  "inputDataset": "file-test-timeout-001",
  "completionWindow": "24h",
  "status": "failed",
  "outputDataset": "",
  "errorDataset": "",
  "createdAt": "1781320519",
  "inProgressAt": "0",
  "expiresAt": "0",
  "finalizingAt": "0",
  "completedAt": "0",
  "failedAt": "1781320519",
  "expiredAt": "0",
  "cancellingAt": "0",
  "cancelledAt": "0",
  "requestCounts": null,
  "usage": null,
  "metadata": {
    "aibrix.console.created_by": "test@aibrix.ai",
    "aibrix.console.display_name": "Provisioning Timeout Test Job",
    "display_name": "Provisioning Timeout Test Job"
  },
  "extraBody": {},
  "errors": [],
  "runtime": null,
  "resourceAllocation": null,
  "modelTemplateRef": null,
  "profile": null,
  "name": "Provisioning Timeout Test Job",
  "createdBy": "test@aibrix.ai",
  "modelTemplateName": "",
  "modelTemplateVersion": "",
  "batchId": "",
  "provisionId": "",
  "errorMessage": "planner: insufficient resources\ninjected error [timeout]: provisioning timeout after 3m (code: DEADLINE_EXCEEDED)",
  "queuedAt": "1781320519",
  "resourcePreparingAt": "0",
  "submittingAt": "0",
  "resourceFailedAt": "1781320519",
  "submitFailedAt": "0",
  "cancelRequestedAt": "0",
  "provision": null,
  "events": [
    {
      "id": "queued",
      "label": "Queued",
      "status": "queued",
      "source": "planner",
      "at": "1781320519",
      "message": "Console accepted the job."
    },
    {
      "id": "resource_failed",
      "label": "Provision failed",
      "status": "resource_failed",
      "source": "planner",
      "at": "1781320519",
      "message": "planner: insufficient resources\ninjected error [timeout]: provisioning timeout after 3m (code: DEADLINE_EXCEEDED)"
    }
  ]
}
```

### Step 3: Query Execution Trace

```bash
# Replace the job ID with the actual job ID
curl http://localhost:8080/api/v1/injection/traces/job_739de962-f830-47cc-b4db-8e9e7631349d
```

**Expected Trace Response:**

```json
{
  "trace": {
    "jobId": "job_739de962-f830-47cc-b4db-8e9e7631349d",
    "startTime": "1781320519",
    "endTime": "1781320519",
    "points": [
      {
        "pointId": "planner.enqueue",
        "timestamp": "1781320519",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.1561093149017987
      },
      {
        "pointId": "store.upsert_job",
        "timestamp": "1781320519",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.3227025207431473
      },
      {
        "pointId": "rm.provision",
        "timestamp": "1781320519",
        "triggered": true,
        "contextSnapshot": {},
        "error": {
          "type": "ERROR_TYPE_TIMEOUT",
          "code": "DEADLINE_EXCEEDED",
          "message": "provisioning timeout after 3m",
          "details": {}
        },
        "templateUsed": "ERROR_TYPE_TIMEOUT",
        "overridesApplied": {
          "duration": "3m",
          "resource_type": "A100 GPU"
        },
        "probabilityRoll": 0.6584111920824858
      }
    ]
  }
}
```

### Step 4: Verify Results

1. **Job Status**: `STATUS_FAILED` indicates the job failed due to the injected error
2. **Error Message**: `provisioning timeout after 3m` matches the configured `overrides`
3. **Trace Record**:
   - `rm.provision` has `triggered` set to `true`
   - `probability_roll` < `probability` (1.0), trigger condition satisfied
   - `overrides_applied` correctly applied custom values

---

## Example 2: Testing Multi-Stage Error Injection

### Scenario Description

Inject errors with different probabilities at multiple stages of job execution to test system behavior under random errors.

### Step 1: Create a Multi-Rule Injection Job

```bash
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "input_dataset": "file-multi-stage-001",
    "endpoint": "/v1/chat/completions",
    "name": "Multi-Stage Injection Test",
    "injection_config": {
      "enabled": true,
      "rules": [
        {
          "point_ref": "planner.enqueue",
          "error_type": "ERROR_TYPE_UNAVAILABLE",
          "probability": 0.3
        },
        {
          "point_ref": "rm.provision",
          "error_type": "ERROR_TYPE_RESOURCE_EXHAUSTED",
          "probability": 0.5,
          "overrides": {"resource_type": "H100 GPU"}
        },
        {
          "point_ref": "batch_client.create_batch",
          "error_type": "ERROR_TYPE_UNAVAILABLE",
          "probability": 0.8
        }
      ]
    }
  }'
```

### Step 2: Run Multiple Times to Observe Different Behaviors

Due to probabilistic triggering, different results will be observed across multiple runs:

**First Run (Failed at Enqueue Stage):**

```json
// Response shows planner.enqueue triggered
{
  "code": 14,
  "message": "create batch: injected error [unavailable]: planner queue unavailable: planning_queue (code: UNAVAILABLE)",
  "details": []
}
```

**Second Run (Failed at Provisioning Stage):**

```json
// Trace shows rm.provision triggered
{
  "trace": {
    "jobId": "job_7907e796-d94e-4475-b76b-1c2071133703",
    "startTime": "1781322724",
    "endTime": "1781322724",
    "points": [
      {
        "pointId": "planner.enqueue",
        "timestamp": "1781322724",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.9780326236270251
      },
      {
        "pointId": "store.upsert_job",
        "timestamp": "1781322724",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.1726237199489347
      },
      {
        "pointId": "rm.provision",
        "timestamp": "1781322724",
        "triggered": true,
        "contextSnapshot": {},
        "error": {
          "type": "ERROR_TYPE_RESOURCE_EXHAUSTED",
          "code": "RESOURCE_EXHAUSTED",
          "message": "no available resources: H100 GPU",
          "details": {}
        },
        "templateUsed": "ERROR_TYPE_RESOURCE_EXHAUSTED",
        "overridesApplied": {
          "resource_type": "H100 GPU"
        },
        "probabilityRoll": 0.3054112859325756
      },
      {
        "pointId": "store.upsert_job",
        "timestamp": "1781322724",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.8667555896800462
      }
    ]
  }
}
```

**Third Run (Failed at Batch Creation Stage):**

```json
// Trace shows batch_client.create_batch triggered
{
  "trace": {
    "jobId": "job_6f3f9a8c-999b-461b-ac31-c69c5754736e",
    "startTime": "1781323704",
    "endTime": "1781323752",
    "points": [
      {
        "pointId": "planner.enqueue",
        "timestamp": "1781323704",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.5862939705095955
      },
      {
        "pointId": "store.upsert_job",
        "timestamp": "1781323704",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.34390934374570414
      },
      {
        "pointId": "rm.provision",
        "timestamp": "1781323704",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.6996724991010442
      },
      {
        "pointId": "store.upsert_provision",
        "timestamp": "1781323704",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.10588722368584613
      },
      {
        "pointId": "store.upsert_job",
        "timestamp": "1781323704",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.5166639286649288
      },
      {
        "pointId": "planner.submit_batch",
        "timestamp": "1781323752",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.5969460505349289
      },
      {
        "pointId": "batch_client.create_batch",
        "timestamp": "1781323752",
        "triggered": true,
        "contextSnapshot": {},
        "error": {
          "type": "ERROR_TYPE_UNAVAILABLE",
          "code": "UNAVAILABLE",
          "message": "batch service unavailable: batch_api",
          "details": {}
        },
        "templateUsed": "ERROR_TYPE_UNAVAILABLE",
        "overridesApplied": {},
        "probabilityRoll": 0.6627944639866115
      },
      {
        "pointId": "store.upsert_job",
        "timestamp": "1781323752",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.32936365322352906
      },
      {
        "pointId": "rm.release",
        "timestamp": "1781323752",
        "triggered": false,
        "contextSnapshot": {},
        "error": null,
        "templateUsed": "ERROR_TYPE_UNSPECIFIED",
        "overridesApplied": {},
        "probabilityRoll": 0.6828558027780979
      }
    ]
  }
}
```

---

## Example 3: Using Global Chaos Injection

### Scenario Description

Configure global chaos injection that affects all newly created jobs (without per-job configuration).

### Step 1: Configure Global Chaos

```bash
curl -X POST http://localhost:8080/api/v1/injection/config \
  -H "Content-Type: application/json" \
  -d '{
    "config": {
      "enabled": true,
      "global_probability": 0.05,
      "point_weights": {
        "rm.provision": 0.10,
        "rm.get_status": 0.08,
        "rm.release": 0.02,
        "planner.submit_batch": 0.15
      },
      "excluded_points": [
        "store.upsert_job",
        "store.get_job",
        "store.upsert_provision",
        "store.get_provision"
      ]
    }
  }'
```

### Step 2: Verify Configuration is Active

```bash
curl http://localhost:8080/api/v1/injection/config
```

**Expected Response:**

```json
{
  "config": {
    "enabled": true,
    "rules": [],
    "excludedPoints": [
      "store.upsert_job",
      "store.get_job",
      "store.upsert_provision",
      "store.get_provision"
    ],
    "globalProbability": 0.05,
    "pointWeights": {
      "planner.submit_batch": 0.15,
      "rm.get_status": 0.08,
      "rm.provision": 0.1,
      "rm.release": 0.02
    }
  }
}
```

### Step 3: Create Multiple Jobs (Without Injection Config)

```bash
# Create job 1
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "input_dataset": "file-chaos-001",
    "endpoint": "/v1/chat/completions",
    "name": "Chaos Test Job 1"
  }'

# Create job 2
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "input_dataset": "file-chaos-002",
    "endpoint": "/v1/chat/completions",
    "name": "Chaos Test Job 2"
  }'
```

### Step 4: Query Traces for Each Job

```bash
curl http://localhost:8080/api/v1/injection/traces/job-001
curl http://localhost:8080/api/v1/injection/traces/job-002
```

### Step 5: Clear Configuration After Testing

```bash
curl -X DELETE http://localhost:8080/api/v1/injection/config
```

---

## Example 4: Testing Crash Recovery Mechanism

### Scenario Description

Use crash injection to test whether the system recovery mechanism works properly. Crash injection is now implemented by using the `crash` error type on injection points that support crash types.

### Step 1: Create a Crash Injection Job

```bash
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "input_dataset": "file-crash-test-001",
    "endpoint": "/v1/chat/completions",
    "name": "Crash Recovery Test",
    "injection_config": {
      "enabled": true,
      "rules": [
        {
          "point_ref": "rm.provision",
          "error_type": "ERROR_TYPE_CRASH",
          "probability": 1.0
        }
      ]
    }
  }'
```

### Step 2: Observe System Logs

```bash
# Check service logs to confirm crash is recorded
# Expected to see similar log:
panic: simulated crash at rm.provision
```

### Step 3: Verify Recovery Mechanism

After restarting the service, check if the job was correctly recovered:

```bash
curl http://localhost:8080/api/v1/jobs/job_efba53d8-20dc-4eca-b2db-2696df4de611
```

### Injection Points Supporting Crash Injection

The following injection points support the `crash` error type:

| Injection Point | Description | Recovery Test Scenario |
|-----------------|-------------|------------------------|
| `planner.enqueue` | Crash during job enqueue | Test job state persistence and recovery replay |
| `planner.recover` | Crash during recovery process | Test recovery resilience and non-terminal job replay |
| `rm.provision` | Crash during resource provisioning | Test provision state persistence and cleanup |
| `rm.release` | Crash during resource release | Test cleanup recovery and orphaned provision handling |
| `store.upsert_job` | Crash during job persistence | Test data persistence and write atomicity |
| `store.upsert_provision` | Crash during provision persistence | Test provision data persistence |

---

## Example 5: Using Wildcard Matching

### Scenario Description

Use wildcard matching to target multiple injection points, simplifying configuration.

### Using Component Wildcard

```bash
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "input_dataset": "file-wildcard-001",
    "endpoint": "/v1/chat/completions",
    "name": "Wildcard Injection Test",
    "injection_config": {
      "enabled": true,
      "rules": [
        {
          "point_ref": "rm.*",
          "error_type": "ERROR_TYPE_UNAVAILABLE",
          "probability": 0.2
        }
      ]
    }
  }'
```

**Effect**: Matches `rm.provision`, `rm.get_status`, `rm.release`, `rm.catalog_lookup`, and all other rm component injection points.

### Using Global Wildcard

```bash
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "input_dataset": "file-wildcard-002",
    "endpoint": "/v1/chat/completions",
    "name": "Global Wildcard Test",
    "injection_config": {
      "enabled": true,
      "rules": [
        {
          "point_ref": "*",
          "error_type": "ERROR_TYPE_UNAVAILABLE",
          "probability": 0.01
        }
      ]
    }
  }'
```

**Effect**: Matches all injection points, each with a 1% probability of triggering.
