# AIBrix Batch Processing

This directory contains the batch processing system for AIBrix, designed to handle large-scale batch inference jobs using a sidecar pattern with vLLM.

## Architecture Overview

The batch processing system consists of several key components:

- **BatchDriver**: Main orchestrator that manages job lifecycle
- **JobManager**: Handles job state management and tracking  
- **RequestProxy**: Bridge between job management and inference execution
- **BatchWorker**: Script-based worker that runs in Kubernetes jobs

## Worker Architecture

The `worker.py` script implements a **sidecar pattern** where:

1. **batch-worker container**: Runs the worker script (exits when complete)
2. **llm-engine container**: Runs vLLM as a persistent service

### Workflow

1. Both containers start simultaneously in the same pod
2. Worker waits for vLLM health check on `localhost:8000`
3. Worker loads its parent Kubernetes Job spec using the Kubernetes API
4. Worker transforms the K8s Job to BatchJob using BatchJobTransformer
5. Worker creates BatchDriver and executes the job
6. Worker tracks job until FINALIZING state
7. Worker exits with status code 0
8. Kubernetes sends SIGTERM to vLLM container
9. Pod completes successfully

## Testing in Kubernetes

### Prerequisites

1. **Docker Images**: Ensure you have the required images built:
   ```bash
   # Build the AIBrix runtime image with batch worker
   make docker-build-runtime
   
   # Or use development images
   docker build -t aibrix/runtime:nightly .
   docker build -t aibrix/vllm-mock:nightly ./vllm-mock
   ```

2. **Kubernetes Cluster**: Have a running cluster (minikube, kind, or full cluster)

3. **Storage Setup**: Configure storage backend (S3, TOS, or local storage)

### Basic Testing

#### 1. Prepare Test Input File and Job Annotations

Create a JSONL file with batch requests and prepare job annotations:

```bash
# Create test input file
cat > /tmp/batch_input.jsonl << 'EOF'
{"custom_id": "request-1", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "Hello world"}]}}
{"custom_id": "request-2", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "What is AI?"}]}}
EOF

# Upload to your storage backend and get the file ID
INPUT_FILE_ID="your-uploaded-file-id"
```

#### 2. Create and Run Batch Job

```bash
# Copy the job template
cp aibrix/metadata/setting/k8s_job_template.yaml /tmp/test-batch-job.yaml

# Edit the template with your specific values
JOB_NAME="test-batch-job-$(date +%s)"
sed -i "s/name: batch-job-template/name: ${JOB_NAME}/" /tmp/test-batch-job.yaml

# Add batch job annotations with your job specification
cat >> /tmp/test-batch-job.yaml << EOF
  annotations:
    batch.job.aibrix.ai/input-file-id: "${INPUT_FILE_ID}"
    batch.job.aibrix.ai/endpoint: "/v1/chat/completions"
    batch.job.aibrix.ai/completion-window: "24h"
    batch.job.aibrix.ai/session-id: "test-session-$(date +%s)"
EOF

# Apply the job
kubectl apply -f /tmp/test-batch-job.yaml
```

#### 3. Monitor Job Progress

```bash
# Watch job status
kubectl get jobs -w

# Check pod logs
POD_NAME=$(kubectl get pods -l app=aibrix-batch --no-headers -o custom-columns=":metadata.name" | head -1)

# Watch worker logs
kubectl logs -f $POD_NAME -c batch-worker

# Watch vLLM logs
kubectl logs -f $POD_NAME -c llm-engine
```

#### 4. Verify Job Completion

```bash
# Check job completion
kubectl get job test-batch-job-* -o wide

# Expected output should show:
# - COMPLETIONS: 1/1
# - STATUS: Complete

# Check pod status
kubectl get pods -l app=aibrix-batch

# Expected output should show:
# - STATUS: Completed
```

### Advanced Testing

#### Job Annotation Configuration

The worker loads job specifications from Kubernetes Job annotations using the `batch.job.aibrix.ai/` prefix:

```yaml
metadata:
  annotations:
    # Required annotations
    batch.job.aibrix.ai/input-file-id: "file-abc123"        # Input file with batch requests
    batch.job.aibrix.ai/endpoint: "/v1/chat/completions"    # API endpoint
    
    # Optional annotations  
    batch.job.aibrix.ai/completion-window: "24h"            # Job completion window (default: 24h)
    batch.job.aibrix.ai/session-id: "session-123"          # Session identifier
    batch.job.aibrix.ai/metadata.key1: "value1"            # Custom metadata (up to 16 pairs)
    batch.job.aibrix.ai/metadata.key2: "value2"

# Environment variables for worker configuration
spec:
  template:
    spec:
      containers:
      - name: batch-worker
        env:
        - name: STORAGE_TYPE
          value: "S3"                 # Optional: Storage backend (AUTO, LOCAL, S3, TOS)
        - name: METASTORE_TYPE  
          value: "S3"                 # Optional: Metastore backend (AUTO, LOCAL, S3, TOS)
```

#### Storage Backend Testing

For **S3 Backend**:
```yaml
env:
- name: STORAGE_TYPE
  value: "S3"
- name: AWS_ACCESS_KEY_ID
  value: "your-access-key"
- name: AWS_SECRET_ACCESS_KEY
  value: "your-secret-key"  
- name: AWS_REGION
  value: "us-west-2"
- name: S3_BUCKET
  value: "your-batch-bucket"
```

For **TOS Backend**:
```yaml
env:
- name: STORAGE_TYPE
  value: "TOS"
- name: TOS_ACCESS_KEY
  value: "your-tos-key"
- name: TOS_SECRET_KEY
  value: "your-tos-secret"
- name: TOS_ENDPOINT
  value: "https://tos-s3-cn-beijing.volces.com"
- name: TOS_REGION
  value: "cn-beijing"
```

#### Custom vLLM Configuration

Modify the vLLM container configuration:

```yaml
- name: llm-engine
  image: aibrix/vllm:nightly
  args:
    - --model
    - microsoft/DialoGPT-medium
    - --port
    - "8000"
    - --served-model-name
    - gpt-3.5-turbo
  resources:
    requests:
      nvidia.com/gpu: 1
    limits:
      nvidia.com/gpu: 1
```

### Debugging

#### Common Issues

1. **Worker fails to start**: Check job annotations and environment variables
   ```bash
   kubectl describe pod $POD_NAME
   kubectl logs $POD_NAME -c batch-worker
   
   # Check job annotations
   kubectl get job $JOB_NAME -o yaml | grep -A 10 annotations
   ```

2. **vLLM health check fails**: Check vLLM container logs
   ```bash
   kubectl logs $POD_NAME -c llm-engine
   ```

3. **Storage access issues**: Verify credentials and permissions
   ```bash
   kubectl logs $POD_NAME -c batch-worker | grep -i storage
   ```

4. **Job hangs**: Check both container logs and job events
   ```bash
   kubectl describe job test-batch-job-*
   kubectl get events --sort-by=.metadata.creationTimestamp
   ```

#### Debug Mode

Enable debug logging by adding:
```yaml
env:
- name: LOG_LEVEL
  value: "DEBUG"
```

### Cleanup

```bash
# Delete test job and pods
kubectl delete job test-batch-job-*
kubectl delete pod -l app=aibrix-batch

# Clean up test files
rm -f /tmp/test-batch-job.yaml /tmp/batch_input.jsonl
```

## Development

### Running Tests

```bash
# Install dependencies
poetry install --no-root --with dev

# Run batch-specific tests
pytest tests/ -k batch

# Run formatting and linting
bash ./scripts/format.sh
```

### Code Structure

```
aibrix/batch/
├── README.md              # This file
├── worker.py              # Main worker script  
├── driver.py              # BatchDriver orchestrator
├── job_manager.py         # Job state management
├── job_driver.py          # Execution bridge
├── scheduler.py           # Job scheduling
├── job_entity/            # Job models and states
│   ├── batch_job.py       # BatchJob schema
│   └── k8s_transformer.py # Kubernetes integration
└── storage/               # Storage abstractions
    ├── adapter.py         # Storage bridge
    └── batch_storage.py   # Batch storage interface
```

The worker script follows the sidecar pattern and integrates with the existing batch processing architecture to provide scalable, Kubernetes-native batch inference capabilities.

## Architecture Changes

### New Kubernetes Integration

The worker now uses **Kubernetes-native job specification** instead of environment variables:

1. **Job Annotations**: Job specifications are stored as Kubernetes annotations using the `batch.job.aibrix.ai/` prefix
2. **Kubernetes API**: Worker connects to the K8s API server using in-cluster configuration 
3. **BatchJobTransformer**: Automatically transforms K8s Job objects to internal BatchJob models
4. **Service Account**: Requires appropriate RBAC permissions to read Job objects

### Required RBAC

The batch worker requires a service account with permissions to read Job objects, as defined in the aibrix/metadata/setting/k8s_job_rbac.yaml

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: job-reader-sa
  namespace: default
---
# Service Account for job pod
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: job-reader-role
  namespace: default
rules:
- apiGroups: ["batch"]
  resources: ["jobs"]
  verbs: ["get"] # Get permissions only
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: job-reader-binding
  namespace: default
subjects:
- kind: ServiceAccount
  name: job-reader-sa
  namespace: default
roleRef:
  kind: Role
  name: job-reader-role
  apiGroup: rbac.authorization.k8s.io
```

Add the service account to your job template:

```yaml
spec:
  template:
    spec:
      serviceAccountName: job-reader-sa
```