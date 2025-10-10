.. _batch_api:

=========
Batch API
=========

The AIBrix Batch API provides an efficient way to process large volumes of LLM inference requests asynchronously. It is fully compatible with the OpenAI Batch API, allowing you to send batches of requests that are processed in the background with results retrieved later.

Overview
--------

Batch processing is ideal for workloads that don't require immediate responses and can benefit from:

- **Cost efficiency**: Process requests during off-peak hours with optimized resource utilization
- **Higher throughput**: Handle large volumes of requests without rate limiting concerns
- **Simplified workflows**: Submit thousands of requests in a single batch operation
- **Guaranteed processing**: Built-in retry mechanisms and failure handling

The Batch API accepts a JSONL (JSON Lines) file containing multiple inference requests, processes them asynchronously using Kubernetes Jobs, and returns results in a corresponding JSONL output file.

Key Features
^^^^^^^^^^^^

- **OpenAI-compatible**: Drop-in replacement for OpenAI's Batch API with identical request/response format
- **Distributed execution**: Leverages Kubernetes Jobs for scalable, fault-tolerant batch processing
- **Metadata server workflow**: Centralized coordination for multi-node batch execution
- **Storage flexibility**: Supports S3, Redis, and local storage backends
- **Request tracking**: Each request has a ``custom_id`` for precise result matching
- **Status monitoring**: Real-time progress tracking with detailed metrics
- **24-hour completion window**: Automatic expiration for long-running batches


Comparison with OpenAI Batch API
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

AIBrix Batch API maintains full compatibility with OpenAI's Batch API while adding enterprise features:

**AIBrix Enhancements:**

- **Self-hosted**: Full control over infrastructure and data
- **Kubernetes-native**: Leverages K8s for scheduling and resource management
- **Flexible storage**: S3, Redis, or local storage backends
- **Distributed execution**: Metadata server coordinates multi-node processing
- **Cost control**: Use existing infrastructure without per-request pricing

**Migration from OpenAI:**

Simply update the ``base_url`` in your OpenAI SDK configuration:

.. code-block:: python

    from openai import OpenAI

    # Before: OpenAI
    # client = OpenAI(api_key="sk-...")

    # After: AIBrix
    client = OpenAI(
        base_url="http://your-aibrix-endpoint/v1",
        api_key="your-key"  # Optional
    )

    # All batch API calls work identically

**Known Differences:**

- **Pricing**: No usage-based pricing; controlled by your infrastructure costs
- **Endpoints**: Currently supports ``/v1/chat/completions`` (more coming soon)
- **Rate Limits**: Determined by your cluster capacity, not API limits

**Current Limitations:**

- Only ``/v1/chat/completions`` endpoint is supported, more endpoints will be added in future releases.
- 24-hour completion window (not configurable)
- No support for batch cancellation via API (use kubectl to delete jobs)


Architecture
------------

Workflow Overview
^^^^^^^^^^^^^^^^^

The Batch API follows a metadata server architecture for distributed processing:

::

    ┌──────────┐
    │  Client  │
    └────┬─────┘
         │ 1. Upload JSONL file
         ▼
    ┌─────────────────┐
    │   Files API     │
    │   (Metadata)    │
    └────┬────────────┘
         │ 2. Create batch job
         ▼
    ┌──────────────────┐         ┌──────────────────┐
    │  Batch API       │────────▶│  Job Scheduler   │
    │  (Metadata)      │         │  (Kubernetes)    │
    └────┬─────────────┘         └────┬─────────────┘
         │                             │ 3. Execute workers
         │ 4. Poll status              ▼
         │                        ┌─────────────┐
         │                        │  K8s Jobs   │
         │                        │  (Workers)  │
         │                        └────┬────────┘
         │                             │ 5. Process requests
         │                             │    & write outputs
         │ 6. Download output          │
         ▼                             ▼
    ┌──────────────────┐         ┌──────────────┐
    │  Files API       │◀────────│   Storage    │
    │  (Metadata)      │         │  (S3/Redis)  │
    └──────────────────┘         └──────────────┘

**Phase Transitions:**

::

    validating → in_progress → finalizing → completed
        ↓            ↓             ↓            ↓
    Preparing    Worker         Collecting  Results
    job files    execution      outputs      ready

**Status Lifecycle:**

1. **validating**: Metadata server validates input file and prepares job configuration
2. **in_progress**: Kubernetes Jobs are executing and processing batch requests
3. **finalizing**: Workers have completed, metadata server is aggregating results
4. **completed**: Output file is ready for download with all results

**Failed/Cancelled States:**

- **failed**: Job execution encountered unrecoverable errors
- **cancelled**: User explicitly cancelled the batch job
- **expired**: Job exceeded the 24-hour completion window

Components
^^^^^^^^^^

1. **Metadata Server**: Coordinates batch job lifecycle, manages files, and tracks progress
2. **Job Scheduler**: Creates and manages Kubernetes Jobs for batch execution
3. **Worker Jobs**: Kubernetes Jobs that process batch requests in parallel
4. **Storage Backend**: S3, Redis, or local filesystem for file storage and job state
5. **Files API**: OpenAI-compatible file upload/download endpoints

Examples
--------

End-to-End Example
^^^^^^^^^^^^^^^^^^

Here's a complete example of processing a batch of chat completions:

**Step 1: Prepare Input File**

Create a file ``batch_input.jsonl`` with your requests:

.. code-block:: json

    {"custom_id": "task-1", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-oss-120b", "messages": [{"role": "system", "content": "You are a helpful assistant."}, {"role": "user", "content": "Explain neural networks."}], "max_tokens": 200}}
    {"custom_id": "task-2", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-oss-120b", "messages": [{"role": "system", "content": "You are a helpful assistant."}, {"role": "user", "content": "What is deep learning?"}], "max_tokens": 200}}
    {"custom_id": "task-3", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-oss-120b", "messages": [{"role": "system", "content": "You are a helpful assistant."}, {"role": "user", "content": "Describe transformers architecture."}], "max_tokens": 200}}

**Step 2: Upload Input File**

.. code-block:: bash

    # Upload the input file
    ENDPOINT="your-aibrix-endpoint:80"

    UPLOAD_RESPONSE=$(curl -X POST http://${ENDPOINT}/v1/files \
      -F "purpose=batch" \
      -F "file=@batch_input.jsonl")

    echo $UPLOAD_RESPONSE
    # {"id":"file-abc123","object":"file","bytes":1024,"created_at":1677610602,"filename":"batch_input.jsonl","purpose":"batch","status":"uploaded"}

    # Extract file ID
    FILE_ID=$(echo $UPLOAD_RESPONSE | jq -r '.id')
    echo "Uploaded file ID: $FILE_ID"

**Step 3: Create Batch Job**

.. code-block:: bash

    # Create batch job
    BATCH_RESPONSE=$(curl -X POST http://${ENDPOINT}/v1/batches \
      -H "Content-Type: application/json" \
      -d "{
        \"input_file_id\": \"${FILE_ID}\",
        \"endpoint\": \"/v1/chat/completions\",
        \"completion_window\": \"24h\"
      }")

    echo $BATCH_RESPONSE

    # Extract batch ID
    BATCH_ID=$(echo $BATCH_RESPONSE | jq -r '.id')
    echo "Created batch ID: $BATCH_ID"

**Step 4: Poll Batch Status**

.. code-block:: bash

    # Poll until completion (with timeout)
    MAX_ATTEMPTS=60
    ATTEMPT=0

    while [ $ATTEMPT -lt $MAX_ATTEMPTS ]; do
      STATUS_RESPONSE=$(curl -s http://${ENDPOINT}/v1/batches/${BATCH_ID})
      STATUS=$(echo $STATUS_RESPONSE | jq -r '.status')

      echo "Attempt $ATTEMPT: Status = $STATUS"

      if [ "$STATUS" = "completed" ]; then
        echo "Batch completed successfully!"
        OUTPUT_FILE_ID=$(echo $STATUS_RESPONSE | jq -r '.output_file_id')
        break
      elif [ "$STATUS" = "failed" ] || [ "$STATUS" = "expired" ] || [ "$STATUS" = "cancelled" ]; then
        echo "Batch processing failed with status: $STATUS"
        exit 1
      fi

      ATTEMPT=$((ATTEMPT + 1))
      sleep 10
    done

    if [ $ATTEMPT -eq $MAX_ATTEMPTS ]; then
      echo "Batch did not complete within timeout"
      exit 1
    fi

**Step 5: Download Results**

.. code-block:: bash

    # Download output file
    curl -o batch_output.jsonl http://${ENDPOINT}/v1/files/${OUTPUT_FILE_ID}/content

    echo "Output saved to batch_output.jsonl"

    # Display results
    cat batch_output.jsonl | jq '.'

**Step 6: Process Results**

.. code-block:: python

    import json

    # Parse output file
    results = {}
    with open('batch_output.jsonl', 'r') as f:
        for line in f:
            output = json.loads(line)
            custom_id = output['custom_id']
            response = output['response']

            if response['status_code'] == 200:
                content = response['body']['choices'][0]['message']['content']
                results[custom_id] = content
                print(f"{custom_id}: {content[:100]}...")
            else:
                print(f"{custom_id}: ERROR {response['status_code']}")

    # Output:
    # task-1: Neural networks are computational models inspired by biological neurons...
    # task-2: Deep learning is a subset of machine learning that uses multi-layer...
    # task-3: The Transformer architecture is a neural network design that relies...

Python SDK Example
^^^^^^^^^^^^^^^^^^

Using the OpenAI Python SDK (works with AIBrix as a drop-in replacement):

.. code-block:: python

    import json
    import time
    from openai import OpenAI

    # Configure client for AIBrix
    client = OpenAI(
        base_url="http://your-aibrix-endpoint:80/v1",
        api_key="dummy-key"  # Replace with actual key if authentication is enabled
    )

    # Step 1: Create batch input file
    batch_requests = [
        {
            "custom_id": f"request-{i}",
            "method": "POST",
            "url": "/v1/chat/completions",
            "body": {
                "model": "gpt-oss-120b",
                "messages": [
                    {"role": "system", "content": "You are a helpful assistant."},
                    {"role": "user", "content": f"Tell me a fact about the number {i}."}
                ],
                "max_tokens": 100
            }
        }
        for i in range(1, 11)  # 10 requests
    ]

    # Write to JSONL file
    with open("batch_requests.jsonl", "w") as f:
        for request in batch_requests:
            f.write(json.dumps(request) + "\n")

    # Step 2: Upload file
    with open("batch_requests.jsonl", "rb") as f:
        batch_file = client.files.create(
            file=f,
            purpose="batch"
        )

    print(f"Uploaded file: {batch_file.id}")

    # Step 3: Create batch
    batch = client.batches.create(
        input_file_id=batch_file.id,
        endpoint="/v1/chat/completions",
        completion_window="24h"
    )

    print(f"Created batch: {batch.id}")

    # Step 4: Wait for completion
    while batch.status not in ["completed", "failed", "expired", "cancelled"]:
        time.sleep(10)
        batch = client.batches.retrieve(batch.id)
        print(f"Status: {batch.status}")

    if batch.status == "completed":
        print(f"Batch completed!")
        print(f"Total requests: {batch.request_counts.total}")
        print(f"Completed: {batch.request_counts.completed}")
        print(f"Failed: {batch.request_counts.failed}")

        # Step 5: Download results
        output_file_id = batch.output_file_id
        result_content = client.files.content(output_file_id)

        # Save results
        with open("batch_results.jsonl", "wb") as f:
            f.write(result_content.content)

        # Process results
        with open("batch_results.jsonl", "r") as f:
            for line in f:
                result = json.loads(line)
                custom_id = result["custom_id"]
                content = result["response"]["body"]["choices"][0]["message"]["content"]
                print(f"{custom_id}: {content}")
    else:
        print(f"Batch failed with status: {batch.status}")

Configuration
-------------

Metadata Server Configuration
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

The metadata server requires configuration for batch processing:

.. code-block:: bash

    # Enable Batch API (note: set to false to enable)
    --disable-batch-api=false

    # Enable Kubernetes Job execution
    --enable-k8s-job=true

    # Optional: Specify custom job template patch
    --k8s-job-patch=/path/to/job_patch.yaml

**Job Template Patch Example:**

Create a YAML file to customize Kubernetes Job behavior:

.. code-block:: yaml

    # k8s_job_patch.yaml
    spec:
      template:
        spec:
          serviceAccountName: batch-job-sa
          containers:
          - name: batch-worker
            env:
            - name: BATCH_WORKER_CONCURRENCY
              value: "10"
            resources:
              requests:
                cpu: "2"
                memory: "4Gi"
              limits:
                cpu: "4"
                memory: "8Gi"
          restartPolicy: OnFailure
      backoffLimit: 3
      ttlSecondsAfterFinished: 3600

Storage Backend Configuration
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Configure storage for batch files and job metadata:

**S3 Storage:**

.. code-block:: python

    from aibrix.metadata.setting import settings
    from aibrix.storage import StorageType

    # Configure S3 storage
    settings.STORAGE_TYPE = StorageType.S3
    settings.S3_BUCKET_NAME = "your-batch-bucket"
    settings.S3_REGION = "us-west-2"

    # Optional: Configure credentials via environment or K8s secret
    # AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY

**Redis Metastore:**

.. code-block:: python

    from aibrix.metadata.setting import settings
    from aibrix.storage import StorageType

    # Configure Redis for job state
    settings.METASTORE_TYPE = StorageType.REDIS

    # Redis connection (via environment variables)
    # REDIS_HOST=localhost
    # REDIS_PORT=6379
    # REDIS_DB=0
    # REDIS_PASSWORD=your-password

**Local Storage (Development Only):**

.. code-block:: python

    from aibrix.metadata.setting import settings
    from aibrix.storage import StorageType

    # Use local filesystem (not recommended for production)
    settings.STORAGE_TYPE = StorageType.LOCAL
    settings.METASTORE_TYPE = StorageType.LOCAL



Best Practices
--------------

Request Optimization
^^^^^^^^^^^^^^^^^^^^

1. **Batch Size**: Process 100-1000 requests per batch for optimal performance
2. **Custom IDs**: Use meaningful identifiers for easy result mapping
3. **Token Limits**: Set appropriate ``max_tokens`` to avoid unnecessary costs
4. **Request Validation**: Validate request format before uploading

Resource Configuration
^^^^^^^^^^^^^^^^^^^^^^

1. **Worker Resources**: Configure CPU/memory based on model size

   .. code-block:: yaml

       resources:
         requests:
           cpu: "2"
           memory: "4Gi"
         limits:
           cpu: "4"
           memory: "8Gi"

2. **Job Concurrency**: Adjust based on available resources (default: 1 concurrent job)

3. **Retry Policy**: Configure backoff limits for transient failures

   .. code-block:: yaml

       backoffLimit: 3
       restartPolicy: OnFailure

Storage Management
^^^^^^^^^^^^^^^^^^

1. **File Cleanup**: Regularly clean up old batch files to save storage

   .. code-block:: bash

       # Delete old files (implement retention policy)
       curl -X DELETE http://${ENDPOINT}/v1/files/${FILE_ID}

2. **Output Retention**: Download and archive important results

3. **Monitoring**: Track storage usage and set alerts for capacity

API Reference
-------------

Files API
^^^^^^^^^

The Files API manages input and output files for batch processing. ENDPOINT is the metadata service endpoint.

.. code-block:: bash

    kubectl port-forward svc/aibrix-metadata-service 8090:8090 -n aibrix-system
    export ENDPOINT=localhost:8090

**Upload File**

.. code-block:: bash

    curl -X POST http://${ENDPOINT}/v1/files \
      -F "purpose=batch" \
      -F "file=@batch_input.jsonl"

    {
      "id": "102983c4-92ef-4de9-a03b-8e05066b16fd",
      "object": "file",
      "bytes": 3104,
      "created_at": 1677610602,
      "filename": "batch_input.jsonl",
      "purpose": "batch",
      "status": "uploaded"
    }

**List File**

.. code-block:: bash

    curl -X GET http://${ENDPOINT}/v1/files/{file_id}

    {
      "id": "102983c4-92ef-4de9-a03b-8e05066b16fd",
      "object": "file",
      "bytes": 3104,
      "created_at": 1760131968,
      "filename": "batch_input.jsonl",
      "purpose": "batch",
      "status": "uploaded",
      "content_type": "application/octet-stream",
      "etag": "e64b86a757f6b6e3bbbe65387158d47a",
      "last_modified": 1760131968
    }


**Download File**

.. code-block:: bash

    curl -X GET http://${ENDPOINT}/v1/files/{file_id}/content

    {"custom_id": "request-1", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-oss-120b", "messages": [{"role": "system", "content": "You are a helpful assistant."},{"role": "user", "content": "Explain quantum computing in simple terms."}],"max_tokens": 1000}}
    {"custom_id": "request-2", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-oss-120b", "messages": [{"role": "system", "content": "You are a creative writing assistant."},{"role": "user", "content": "Write a short story about a robot discovering emotions."}],"max_tokens": 1000}}
    {"custom_id": "request-3", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-oss-120b", "messages": [{"role": "system", "content": "You are a code reviewer."},{"role": "user", "content": "Review this Python function: def fibonacci(n): return n if n <= 1 else fibonacci(n-1) + fibonacci(n-2)"}],"max_tokens": 1000}}
    {"custom_id": "request-4", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-oss-120b", "messages": [{"role": "system", "content": "You are a cooking instructor."},{"role": "user", "content": "How do I make perfect scrambled eggs?"}],"max_tokens": 1000}}
    {"custom_id": "request-5", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-oss-120b", "messages": [{"role": "system", "content": "You are a travel advisor."},{"role": "user", "content": "What are the top 5 must-see attractions in Tokyo for first-time visitors?"}],"max_tokens": 1000}}
    ...


**Response:** Raw file content (JSONL format)

Batch API
^^^^^^^^^

The Batch API manages batch job lifecycle.

**Create Batch**

.. code-block:: bash

    curl -X POST http://${ENDPOINT}/v1/batches \
      -H "Content-Type: application/json" \
      -d '{
        "input_file_id": "102983c4-92ef-4de9-a03b-8e05066b16fd",
        "endpoint": "/v1/chat/completions",
        "completion_window": "24h"
      }'

    {
      "id": "6f646d68-1314-42f9-907b-b50a88061a9f",
      "object": "batch",
      "endpoint": "/v1/chat/completions",
      "errors": null,
      "input_file_id": "102983c4-92ef-4de9-a03b-8e05066b16fd",
      "completion_window": "24h",
      "status": "created",
      "output_file_id": null,
      "error_file_id": null,
      "created_at": 1760132899,
      "in_progress_at": null,
      "expires_at": 1760219299,
      "finalizing_at": null,
      "completed_at": null,
      "failed_at": null,
      "expired_at": null,
      "cancelling_at": null,
      "cancelled_at": null,
      "request_counts": null,
      "metadata": null
    }

**Get Batch Status**

.. code-block:: bash

    curl -X GET http://${ENDPOINT}/v1/batches/{batch_id}

    {
      "id": "6f646d68-1314-42f9-907b-b50a88061a9f",
      "object": "batch",
      "endpoint": "/v1/chat/completions",
      "errors": null,
      "input_file_id": "102983c4-92ef-4de9-a03b-8e05066b16fd",
      "completion_window": "24h",
      "status": "completed",
      "output_file_id": "4d4c4f0d-43e2-3a76-8c44-06b95b5afc08",
      "error_file_id": "eca1882e-5bf2-3c23-9b03-f54f98558302",
      "created_at": 1760132899,
      "in_progress_at": 1760132899,
      "expires_at": 1760219299,
      "finalizing_at": 1760132909,
      "completed_at": 1760132909,
      "failed_at": null,
      "expired_at": null,
      "cancelling_at": null,
      "cancelled_at": null,
      "request_counts": {
        "total": 10,
        "completed": 10,
        "failed": 0
      },
      "metadata": null
    }

**List Batches**

.. code-block:: bash

    curl -X GET http://${ENDPOINT}/v1/batches

    {
      "object": "list",
      "data": [
        {
          "id": "6f646d68-1314-42f9-907b-b50a88061a9f",
          "object": "batch",
          "endpoint": "/v1/chat/completions",
          "errors": null,
          "input_file_id": "102983c4-92ef-4de9-a03b-8e05066b16fd",
          "completion_window": "24h",
          "status": "completed",
          "output_file_id": "4d4c4f0d-43e2-3a76-8c44-06b95b5afc08",
          "error_file_id": "eca1882e-5bf2-3c23-9b03-f54f98558302",
          "created_at": 1760132899,
          "in_progress_at": 1760132899,
          "expires_at": 1760219299,
          "finalizing_at": 1760132909,
          "completed_at": 1760132909,
          "failed_at": null,
          "expired_at": null,
          "cancelling_at": null,
          "cancelled_at": null,
          "request_counts": {
            "total": 10,
            "completed": 10,
            "failed": 0
          },
          "metadata": null
        }
      ],
      "first_id": "6f646d68-1314-42f9-907b-b50a88061a9f",
      "last_id": "6f646d68-1314-42f9-907b-b50a88061a9f",
      "has_more": false
    }

Input File Format
^^^^^^^^^^^^^^^^^

Input files must be in JSONL format with one request per line. Each request requires:

- ``custom_id``: Unique identifier for matching results (required)
- ``method``: HTTP method, typically "POST" (required)
- ``url``: Endpoint path, e.g., "/v1/chat/completions" (required)
- ``body``: Request payload matching the endpoint's format (required)

**Example batch_input.jsonl:**

.. code-block:: json

    {"custom_id": "request-1", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-3.5-turbo", "messages": [{"role": "system", "content": "You are a helpful assistant."}, {"role": "user", "content": "What is AI?"}], "max_tokens": 100}}
    {"custom_id": "request-2", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-3.5-turbo", "messages": [{"role": "system", "content": "You are a helpful assistant."}, {"role": "user", "content": "Explain quantum computing."}], "max_tokens": 150}}
    {"custom_id": "request-3", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-3.5-turbo", "messages": [{"role": "system", "content": "You are a helpful assistant."}, {"role": "user", "content": "What is machine learning?"}], "max_tokens": 100}}


Output File Format
^^^^^^^^^^^^^^^^^^

Output files are in JSONL format with one result per line matching each input request:

**Example batch_output.jsonl:**

.. code-block:: json

    {"id": "batch-def456-0", "custom_id": "request-1", "response": {"status_code": 200, "request_id": "req_001", "body": {"id": "chatcmpl-001", "object": "chat.completion", "created": 1677610602, "model": "gpt-3.5-turbo", "choices": [{"index": 0, "message": {"role": "assistant", "content": "AI stands for Artificial Intelligence..."}, "finish_reason": "stop"}]}}}
    {"id": "batch-def456-1", "custom_id": "request-2", "response": {"status_code": 200, "request_id": "req_002", "body": {"id": "chatcmpl-002", "object": "chat.completion", "created": 1677610603, "model": "gpt-3.5-turbo", "choices": [{"index": 0, "message": {"role": "assistant", "content": "Quantum computing uses quantum mechanics..."}, "finish_reason": "stop"}]}}}
    {"id": "batch-def456-2", "custom_id": "request-3", "response": {"status_code": 200, "request_id": "req_003", "body": {"id": "chatcmpl-003", "object": "chat.completion", "created": 1677610604, "model": "gpt-3.5-turbo", "choices": [{"index": 0, "message": {"role": "assistant", "content": "Machine learning is a subset of AI..."}, "finish_reason": "stop"}]}}}

Each output line contains:

- ``id``: Unique result identifier
- ``custom_id``: Matches the input request's custom_id
- ``response``: Contains ``status_code``, ``request_id``, and ``body`` with the actual result
