# Error Injection Usage Guide

This guide provides practical instructions for using the AIBrix Console error injection system for testing, chaos engineering, and debugging.

## Overview

The error injection system allows you to inject controlled errors into the batch processing pipeline. There are two main usage modes:

1. **Per-Request Injection**: Target specific jobs with custom error scenarios
2. **Global Chaos Injection**: Apply random errors across all jobs for chaos testing

---

## 1. Per-Request Injection (Job-Level)

Per-request injection is the recommended approach for targeted testing of specific job behaviors. You specify injection configuration directly in the job creation request.

### Basic Example

To inject a timeout error during resource provisioning for a specific job:

```bash
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "input_dataset": "file-abc123",
    "endpoint": "/v1/chat/completions",
    "name": "Test Batch with Timeout",
    "injection_config": {
      "enabled": true,
      "job_id": "job_test_timeout_123",
      "rules": [
        {
          "point_ref": "rm.provision",
          "error_type": "ERROR_TYPE_TIMEOUT",
          "probability": 1.0,
          "overrides": {"duration": "2m"}
        }
      ]
    }
  }'
```

### Injection Config Fields

| Field | Type | Description |
|-------|------|-------------|
| `enabled` | bool | Must be `true` to activate injection |
| `job_id` | string | Job identifier for trace lookup (optional, auto-generated if omitted) |
| `rules` | array | List of injection rules to apply |
| `global_probability` | float | Default probability for rules without explicit probability (0.0-1.0) |

### Injection Rule Fields

| Field | Type | Description |
|-------|------|-------------|
| `point_ref` | string | Injection point ID (e.g., `rm.provision`) or wildcard pattern |
| `error_type` | string | Error type enum (e.g., `ERROR_TYPE_TIMEOUT`) |
| `probability` | float | Trigger chance (0.0-1.0); 1.0 = always inject |
| `overrides` | map | Custom placeholder values for error message template |

### Example Scenarios

#### Testing Provisioning Timeout

```json
{
  "injection_config": {
    "enabled": true,
    "rules": [
      {
        "point_ref": "rm.provision",
        "error_type": "ERROR_TYPE_TIMEOUT",
        "probability": 1.0,
        "overrides": {"duration": "5m", "resource_type": "gpu-a100"}
      }
    ]
  }
}
```

Result: Job will fail with error message "provisioning timeout after 5m".

#### Testing Resource Exhaustion

```json
{
  "injection_config": {
    "enabled": true,
    "rules": [
      {
        "point_ref": "rm.provision",
        "error_type": "ERROR_TYPE_RESOURCE_EXHAUSTED",
        "probability": 1.0,
        "overrides": {"resource_type": "gpu-memory"}
      }
    ]
  }
}
```

Result: Job will fail with "no available resources: gpu-memory".

#### Testing Batch Submission Failure

```json
{
  "injection_config": {
    "enabled": true,
    "rules": [
      {
        "point_ref": "planner.submit_batch",
        "error_type": "ERROR_TYPE_UNAVAILABLE",
        "probability": 1.0,
        "overrides": {"client": "batch_api", "service": "metadata-service"}
      }
    ]
  }
}
```

#### Multiple Injection Points

Inject errors at multiple stages with different probabilities:

```json
{
  "injection_config": {
    "enabled": true,
    "rules": [
      {
        "point_ref": "rm.provision",
        "error_type": "ERROR_TYPE_TIMEOUT",
        "probability": 0.5
      },
      {
        "point_ref": "batch_client.create_batch",
        "error_type": "ERROR_TYPE_UNAVAILABLE",
        "probability": 0.3
      }
    ]
  }
}
```

#### Using Global Probability

When `global_probability` is set, rules without explicit probability inherit it:

```json
{
  "injection_config": {
    "enabled": true,
    "global_probability": 0.1,
    "rules": [
      {"point_ref": "rm.provision", "error_type": "ERROR_TYPE_TIMEOUT"},
      {"point_ref": "planner.submit_batch", "error_type": "ERROR_TYPE_UNAVAILABLE"}
    ]
  }
}
```

Both rules will use 10% probability.

---

## 2. Global Chaos Injection

Global injection applies to all jobs that don't have per-job injection config. Use this for chaos engineering across the entire system.

### Available RPCs

The `InjectionService` provides these gRPC/HTTP endpoints:

| RPC | HTTP Path | Description |
|-----|-----------|-------------|
| `ListInjectionPoints` | `GET /api/v1/injection/points` | Discover all registered injection points |
| `GetInjectionConfig` | `GET /api/v1/injection/config` | Read current global config |
| `SetInjectionConfig` | `POST /api/v1/injection/config` | Update global config |
| `ClearInjectionConfig` | `DELETE /api/v1/injection/config` | Disable all injection |
| `GetInjectionTrace` | `GET /api/v1/injection/traces/{job_id}` | Query trace for a specific job |

### Discover Injection Points

```bash
# List all available injection points
curl http://localhost:8080/api/v1/injection/points

# Using grpcurl
grpcurl -plaintext localhost:50060 console.v1.InjectionService/ListInjectionPoints
```

Response includes all injection points with their supported error types and template placeholders.

### Set Global Injection Config

```bash
curl -X POST http://localhost:8080/api/v1/injection/config \
  -H "Content-Type: application/json" \
  -d '{
    "config": {
      "enabled": true,
      "rules": [
        {
          "point_ref": "rm.provision",
          "error_type": "ERROR_TYPE_TIMEOUT",
          "probability": 0.1,
          "overrides": {"duration": "3m"}
        },
        {
          "point_ref": "planner.submit_batch",
          "error_type": "ERROR_TYPE_UNAVAILABLE",
          "probability": 0.05
        }
      ],
      "excluded_points": ["store.upsert_job", "store.get_job"],
      "global_probability": 0.02
    }
  }'
```

### Global Config Fields

| Field | Type | Description |
|-------|------|-------------|
| `enabled` | bool | Master switch for global injection |
| `rules` | array | Global injection rules |
| `excluded_points` | array | Points that will never have errors injected (safety) |
| `global_probability` | float | Default probability for chaos mode (applies when rule has no probability) |
| `point_weights` | map | Per-point probability overrides (point_id -> probability) |

### Using Point Weights

`point_weights` allows setting different probabilities per injection point without explicit rules:

```json
{
  "enabled": true,
  "global_probability": 0.05,
  "point_weights": {
    "rm.provision": 0.15,
    "batch_client.create_batch": 0.08,
    "planner.enqueue": 0.02
  }
}
```

This applies 15% probability to `rm.provision`, 8% to `batch_client.create_batch`, 2% to `planner.enqueue`, and 5% to all other points.

### When to Use Global Chaos vs Per-Request Injection

| Scenario | Recommended Approach |
|----------|---------------------|
| Targeted testing of specific error handling | Per-Request Injection |
| Testing complete execution path for specific job | Per-Request Injection |
| System-wide chaos engineering | Global Chaos Injection |
| Testing overall system resilience | Global Chaos Injection |
| Validating recovery under random errors | Global Chaos Injection |
| Deterministic testing in integration tests | Per-Request Injection (probability=1.0) |

### Get Current Config

```bash
curl http://localhost:8080/api/v1/injection/config

# grpcurl
grpcurl -plaintext localhost:50060 console.v1.InjectionService/GetInjectionConfig
```

### Clear Global Config (Disable All Injection)

```bash
curl -X DELETE http://localhost:8080/api/v1/injection/config

# grpcurl
grpcurl -plaintext localhost:50060 console.v1.InjectionService/ClearInjectionConfig
```

---

## 3. Querying Job Trace

After a job with injection config completes (or fails), query its execution trace to verify what happened.

### Query Trace

```bash
# HTTP API
curl http://localhost:8080/api/v1/injection/traces/job_abc123

# grpcurl
grpcurl -plaintext -d '{"job_id": "job_abc123"}' \
  localhost:9090 console.v1.InjectionService/GetInjectionTrace
```

### Trace Structure

```json
{
  "trace": {
    "job_id": "job_abc123",
    "start_time": 1718100000,
    "end_time": 1718100030,
    "points": [
      {
        "point_id": "console.create_job",
        "timestamp": 1718100000,
        "triggered": false,
        "probability_roll": 0.85
      },
      {
        "point_id": "planner.enqueue",
        "timestamp": 1718100001,
        "triggered": false,
        "probability_roll": 0.72
      },
      {
        "point_id": "rm.provision",
        "timestamp": 1718100002,
        "triggered": true,
        "error": {
          "type": "ERROR_TYPE_TIMEOUT",
          "code": "DEADLINE_EXCEEDED",
          "message": "provisioning timeout after 2m"
        },
        "template_used": "ERROR_TYPE_TIMEOUT",
        "overrides_applied": {"duration": "2m"},
        "probability_roll": 0.03
      }
    ]
  }
}
```

### Understanding Trace Fields

| Field | Description |
|-------|-------------|
| `job_id` | The job being traced |
| `start_time` / `end_time` | Unix timestamp of first/last injection point |
| `points` | Array of all injection points evaluated (chronological order) |
| `point_id` | Which injection point was evaluated |
| `triggered` | Whether an error was actually injected |
| `probability_roll` | Random value (0-1) that was compared against probability threshold |
| `error` | The injected error details (only when triggered=true) |
| `template_used` | Error type template that was applied |
| `overrides_applied` | Custom values used for message rendering |

### Interpreting Results

- **triggered=false, probability_roll > probability**: The random roll exceeded the threshold, so no error was injected.
- **triggered=true, probability_roll <= probability**: The roll was below threshold, error was injected.
- **excluded point**: If a point is in `excluded_points`, it is always skipped (no record created).

---

## 4. Available Injection Points

All injection points are organized by pipeline component. Each point has specific error type templates.

### Console Layer

| Point ID | Description | Supported Error Types |
|----------|-------------|----------------------|
| `console.create_job` | Job creation handler | `invalid_argument`, `unavailable`, `permission_denied` |
| `console.get_job` | Job retrieval handler | `not_found`, `unavailable` |
| `console.cancel_job` | Job cancellation handler | `not_found`, `permission_denied`, `unavailable` |
| `console.list_jobs` | Job listing handler | `unavailable` |
| `console.upload_file` | File upload handler | `invalid_argument`, `unavailable` |
| `console.download_file` | File download handler | `not_found`, `unavailable` |

### Planner

| Point ID | Description | Supported Error Types |
|----------|-------------|----------------------|
| `planner.enqueue` | Job enqueue to planning queue | `invalid_argument`, `unavailable`, `crash` |
| `planner.plan` | Job planning logic | `internal`, `resource_exhausted` |
| `planner.submit_batch` | Submit job to batch system | `unavailable`, `invalid_argument` |
| `planner.cancel` | Cancel planned job | `not_found`, `unavailable` |
| `planner.persist` | Persist job state to store | `unavailable` |
| `planner.recover` | Recover planner state | `unavailable`, `internal`, `crash` |

### Resource Manager

| Point ID | Description | Supported Error Types |
|----------|-------------|----------------------|
| `rm.provision` | Resource provisioning | `unavailable`, `resource_exhausted`, `timeout`, `crash` |
| `rm.get_status` | Get provisioning status | `not_found`, `unavailable` |
| `rm.release` | Release provisioned resources | `unavailable`, `timeout`, `crash` |
| `rm.catalog_lookup` | Lookup resource in catalog | `not_found`, `unavailable` |

### Batch Client

| Point ID | Description | Supported Error Types |
|----------|-------------|----------------------|
| `batch_client.create_batch` | Create batch job | `unavailable`, `invalid_argument`, `timeout` |
| `batch_client.get_batch` | Get batch job status | `not_found`, `unavailable` |
| `batch_client.cancel_batch` | Cancel batch job | `not_found`, `unavailable` |
| `batch_client.list_batches` | List batch jobs | `unavailable` |

### Store Layer

| Point ID | Description | Supported Error Types |
|----------|-------------|----------------------|
| `store.upsert_job` | Upsert job to store | `unavailable`, `internal`, `crash` |
| `store.get_job` | Get job from store | `not_found`, `unavailable` |
| `store.list_jobs` | List jobs from store | `unavailable` |
| `store.upsert_provision` | Upsert provision to store | `unavailable`, `crash` |
| `store.get_provision` | Get provision from store | `not_found`, `unavailable` |

### Crash Injection

Crash injection simulates process/goroutine crashes via `panic()` to test recovery mechanisms. Crash is an error type that can be injected at specific injection points.

The following injection points support the `crash` error type:

| Injection Point | Description | Recovery Test Scenario |
|-----------------|-------------|------------------------|
| `planner.enqueue` | Crash during job enqueue | Tests job state persistence and recovery replay |
| `planner.recover` | Crash during recovery | Tests recovery resilience and non-terminal job replay |
| `rm.provision` | Crash during provisioning | Tests provision state persistence and cleanup |
| `rm.release` | Crash during release | Tests cleanup recovery and orphaned provision handling |
| `store.upsert_job` | Crash during job persistence | Tests data durability and write atomicity |
| `store.upsert_provision` | Crash during provision persistence | Tests provision data durability |

To inject a crash, use the `crash` error type on one of the above injection points:

```json
{
  "injection_config": {
    "enabled": true,
    "rules": [
      {
        "point_ref": "planner.enqueue",
        "error_type": "ERROR_TYPE_CRASH",
        "probability": 1.0
      }
    ]
  }
}
```

**Warning**: Crash injection causes actual process/goroutine termination. Use only in controlled test environments.

---

## 5. Error Types

Each error type maps to a specific gRPC/HTTP status code and has pre-defined message templates.

| Error Type | gRPC Code | Use Case |
|------------|-----------|----------|
| `ERROR_TYPE_TIMEOUT` | `DEADLINE_EXCEEDED` | Test timeout handling in provisioning, batch operations |
| `ERROR_TYPE_UNAVAILABLE` | `UNAVAILABLE` | Test service unavailable scenarios, network issues |
| `ERROR_TYPE_INVALID_ARGUMENT` | `INVALID_ARGUMENT` | Test validation logic, malformed requests |
| `ERROR_TYPE_NOT_FOUND` | `NOT_FOUND` | Test missing resources (jobs, batches, provisions) |
| `ERROR_TYPE_PERMISSION_DENIED` | `PERMISSION_DENIED` | Test authorization failures |
| `ERROR_TYPE_RESOURCE_EXHAUSTED` | `RESOURCE_EXHAUSTED` | Test quota limits, resource allocation failures |
| `ERROR_TYPE_INTERNAL` | `INTERNAL` | Test internal error handling |
| `ERROR_TYPE_CRASH` | `CRASH` | Test recovery mechanisms (causes actual panic) |

### Default Placeholders

Each injection point template has default placeholder values. You can override them using the `overrides` field:

**rm.provision templates:**

- `timeout`: `duration="10m"`, `resource_type="gpu"`
- `unavailable`: `component="provisioner"`
- `resource_exhausted`: `resource_type="gpu"`

**planner.submit_batch templates:**

- `unavailable`: `client="batch_client"`
- `invalid_argument`: `reason="invalid job spec"`

**batch_client.create_batch templates:**

- `unavailable`: `service="batch_api"`
- `invalid_argument`: `reason="invalid spec"`
- `timeout`: `duration="60s"`

### Error Message Template Examples

Each injection point has pre-defined message templates for each error type:

**rm.provision - timeout:**
```
provisioning timeout after {{.duration}}
```
Default placeholder: `duration: "10m"`

**rm.provision - resource_exhausted:**
```
no available resources: {{.resource_type}}
```
Default placeholder: `resource_type: "gpu"`

**console.create_job - invalid_argument:**
```
invalid job spec: {{.reason}}
```
Default placeholder: `reason: "missing required field"`

**console.create_job - permission_denied:**
```
permission denied: {{.action}} on {{.resource}}
```
Default placeholders: `action: "create"`, `resource: "job"`

---

## 6. Best Practices

### Use Per-Request for Targeted Testing

Per-request injection is ideal for:

- Testing specific error handling paths
- Debugging known failure scenarios
- Validating retry/recovery mechanisms
- CI/CD pipeline integration tests

**Best Practices:**

- **Deterministic testing**: Set `probability: 1.0` to ensure errors always trigger
- **Targeted testing**: Select precise `point_ref` to test specific logic
- **Custom messages**: Use `overrides` to customize error message parameters for easier identification
- **Verify traces**: Query traces after injection to verify behavior matches expectations

Example: Testing resource provisioning timeout handling

```json
{
  "injection_config": {
    "enabled": true,
    "job_id": "test-rm-provision-timeout",
    "rules": [
      {
        "point_ref": "rm.provision",
        "error_type": "ERROR_TYPE_TIMEOUT",
        "probability": 1.0,
        "overrides": {"duration": "3m"}
      }
    ]
  }
}
```

### Use Global Chaos for Broad Testing

Global injection is ideal for:

- Chaos engineering across the entire system
- Stress testing with random failures
- Testing system resilience under unpredictable conditions

**Best Practices:**

- **Start low**: Initial `global_probability` should be 0.01-0.05
- **Protect critical points**: Add storage layer and state persistence points to `excluded_points`
- **Gradual increase**: Increase probability gradually after verifying system stability
- **Monitor alerts**: Monitor error rates and recovery time during chaos testing
- **Clean up regularly**: Call `ClearInjectionConfig` after testing completes

```json
// Chaos mode: 5% random failures everywhere
{
  "enabled": true,
  "global_probability": 0.05,
  "excluded_points": ["store.upsert_job", "store.get_job"]
}
```

Recommended excluded points configuration:

```json
{
  "excluded_points": [
    "store.upsert_job",
    "store.get_job",
    "store.upsert_provision",
    "store.get_provision"
  ]
}
```

### Always Query Trace After Injection

Verify the injection behavior by checking the trace:

```bash
# After job creation with injection
curl http://localhost:8080/api/v1/injection/traces/{job_id}
```

This confirms:
- Which points were evaluated
- Whether errors triggered correctly
- The random values that determined outcomes

### Start with Low Probabilities

When using chaos mode:

1. Start with very low probability (1-2%)
2. Monitor system behavior
3. Gradually increase if system handles errors well
4. Never exceed 20% in production-like environments

### Exclude Critical Points

Always exclude points that should not fail:

```json
{
  "excluded_points": [
    "store.upsert_job",     // Job persistence must succeed
    "store.get_job",        // Job retrieval must succeed
    "console.create_job"    // Request entry must succeed
  ]
}
```

### Use Crash Points Carefully

Crash injection:

- Causes actual process/goroutine termination
- Should only be used in isolated test environments
- Requires proper recovery testing setup
- Never use in production or shared test environments

**Testing Recovery Mechanisms:**

Crash injection is used to verify recovery mechanisms:

1. **Set crash point**: Choose an injection point that supports `crash` error type
2. **Configure job**: Create a job with crash injection
3. **Observe crash**: System logs should show "simulated crash"
4. **Verify recovery**: Check if job state is correctly recovered from storage

```json
{
  "injection_config": {
    "enabled": true,
    "rules": [
      {
        "point_ref": "planner.enqueue",
        "error_type": "ERROR_TYPE_CRASH",
        "probability": 1.0
      }
    ]
  }
}
```

### Trace Query and Analysis

- **Query immediately after injection**: Verify if injection triggered at expected points
- **Check probability_roll**: Understand random probability calculation
- **Analyze execution order**: Confirm pipeline execution flow
- **Compare configuration**: Verify `overrides_applied` are correctly applied

### Safety Considerations

- **Disable in production**: Error injection should not be enabled in production environments
- **Environment isolation**: Chaos testing should be conducted in isolated test environments
- **Rollback mechanism**: Call `ClearInjectionConfig` immediately after testing completes
- **Team notification**: Notify relevant teams before chaos testing to avoid misunderstandings

---

## 7. Quick Reference

### Per-Request Injection Example

```bash
curl -X POST http://localhost:8080/api/v1/jobs -H "Content-Type: application/json" -d '{
  "input_dataset": "file-abc",
  "endpoint": "/v1/chat/completions",
  "name": "Injection Test",
  "injection_config": {
    "enabled": true,
    "rules": [{"point_ref": "rm.provision", "error_type": "ERROR_TYPE_TIMEOUT", "probability": 1.0}]
  }
}'
```

### Global Chaos Setup

```bash
curl -X POST http://localhost:8080/api/v1/injection/config \
  -H "Content-Type: application/json" \
  -d '{
    "config": {
      "enabled": true,
      "global_probability": 0.05,
      "excluded_points": ["store.upsert_job"]
    }
  }'
```

### Disable Global Injection

```bash
curl -X DELETE http://localhost:8080/api/v1/injection/config
```

### Query Trace

```bash
curl http://localhost:8080/api/v1/injection/traces/{job_id}
```

### List Injection Points

```bash
curl http://localhost:8080/api/v1/injection/points
```