# Storage Tests

This directory contains comprehensive tests for the aibrix storage module. The tests are designed to validate both local filesystem storage and cloud storage implementations like AWS S3.

## Test Structure

- `test_base_storage.py` - Core storage functionality tests that run against all storage implementations
- `test_local_storage.py` - LocalStorage specific tests
- `test_s3_storage.py` - S3Storage specific tests (requires S3 configuration)
- `test_redis_storage.py` - Redis storage tests (integration tests require Redis server)
- `test_factory.py` - Storage factory tests
- `conftest.py` - Pytest fixtures and configuration

## Running Tests

### Local Storage Tests Only

By default, tests will run against local storage:

```bash
# Run all storage tests (local only)
pytest tests/storage/

# Run specific test file
pytest tests/storage/test_local_storage.py

# Run with verbose output
pytest tests/storage/ -v
```

### Including S3 Tests

To run S3 tests, you need to configure AWS credentials and specify a test bucket.

### Including Redis Tests

Redis storage tests are conditionally enabled based on environment variables. Integration tests that require a Redis server are automatically skipped unless Redis is configured.

## S3 Test Configuration

### Prerequisites

1. **AWS Account**: You need access to an AWS account with S3 permissions
2. **Test Bucket**: Create a dedicated S3 bucket for testing (e.g., `my-aibrix-test-bucket`)
3. **AWS Credentials**: Configure AWS credentials using one of the methods below

### Method 1: Environment Variables

Set the following environment variables:

```bash
export AWS_ACCESS_KEY_ID="your-access-key-id"
export AWS_SECRET_ACCESS_KEY="your-secret-access-key"
export AWS_DEFAULT_REGION="us-east-1"  # or your preferred region
export AIBRIX_TEST_S3_BUCKET="your-test-bucket-name"
```

### Method 2: AWS Credentials File

Create or update `~/.aws/credentials`:

```ini
[default]
aws_access_key_id = your-access-key-id
aws_secret_access_key = your-secret-access-key
```

And `~/.aws/config`:

```ini
[default]
region = us-east-1
```

Then set the test bucket environment variable:

```bash
export AIBRIX_TEST_S3_BUCKET="your-test-bucket-name"
```

### Method 3: AWS CLI Configuration

If you have AWS CLI installed:

```bash
aws configure
# Follow prompts to enter credentials

# Set test bucket
export AIBRIX_TEST_S3_BUCKET="your-test-bucket-name"
```

### Running S3 Tests

Once configured, S3 tests will automatically be included:

```bash
# Run all storage tests including S3
pytest tests/storage/ -v

# Run only S3-specific tests
pytest tests/storage/test_s3_storage.py -v

# Run base tests against both local and S3 storage
pytest tests/storage/test_base_storage.py -v
```

## Test Bucket Requirements

### Permissions

Your AWS credentials need the following S3 permissions for the test bucket:

```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "s3:GetObject",
                "s3:PutObject",
                "s3:DeleteObject",
                "s3:ListBucket",
                "s3:GetObjectSize",
                "s3:CopyObject",
                "s3:CreateMultipartUpload",
                "s3:UploadPart",
                "s3:CompleteMultipartUpload",
                "s3:AbortMultipartUpload"
            ],
            "Resource": [
                "arn:aws:s3:::your-test-bucket-name",
                "arn:aws:s3:::your-test-bucket-name/*"
            ]
        }
    ]
}
```

### Bucket Setup

Create your test bucket:

```bash
# Using AWS CLI
aws s3 mb s3://your-test-bucket-name

# Or create through AWS Console
```

**Important**: Use a dedicated test bucket as tests will create and delete objects.

## Test Behavior

### Automatic Detection

The test framework automatically detects available storage configurations:

1. **Local Storage**: Always available
2. **S3 Storage**: Only enabled if:
   - AWS credentials are configured (env vars or `~/.aws/` files)
   - `AIBRIX_TEST_S3_BUCKET` environment variable is set

### Test Cleanup

All tests clean up after themselves by deleting created objects. However, in case of test failures, you may need to manually clean up the test bucket.

### Test Isolation

Each test uses unique object keys to avoid conflicts when running tests in parallel.

## Troubleshooting

### S3 Tests Are Skipped

If you see messages like "S3 credentials not available" or "AIBRIX_TEST_S3_BUCKET environment variable not set", check:

1. AWS credentials are properly configured
2. `AIBRIX_TEST_S3_BUCKET` environment variable is set
3. The bucket exists and is accessible
4. Your credentials have the required permissions

### Test Failures

Common issues and solutions:

1. **Access Denied**: Check bucket permissions and credentials
2. **Bucket Not Found**: Verify bucket name and region
3. **Connection Timeout**: Check network connectivity to AWS
4. **Rate Limiting**: AWS may throttle requests; tests include retry logic

### Debug Mode

Run tests with more verbose output:

```bash
pytest tests/storage/ -v -s --log-cli-level=DEBUG
```

## Example Test Session

```bash
# Complete test session with S3
export AWS_ACCESS_KEY_ID="AKIA..."
export AWS_SECRET_ACCESS_KEY="..."
export AWS_DEFAULT_REGION="us-east-1"
export AIBRIX_TEST_S3_BUCKET="my-test-bucket"

pytest tests/storage/ -v
```

Expected output:
```
tests/storage/test_base_storage.py::TestBaseStorageFunctionality::test_put_and_get_string[local_storage] PASSED
tests/storage/test_base_storage.py::TestBaseStorageFunctionality::test_put_and_get_string[s3_storage] PASSED
tests/storage/test_local_storage.py::TestLocalStorage::test_local_storage_initialization PASSED
tests/storage/test_s3_storage.py::TestS3Storage::test_s3_storage_initialization PASSED
tests/storage/test_redis_storage.py::test_redis_storage_creation PASSED
...
```

This indicates tests are running against both local and S3 storage implementations.

## Redis Test Configuration

### Prerequisites

Redis storage tests include both unit tests and integration tests:

- **Unit Tests**: Always run, test Redis storage creation and configuration
- **Integration Tests**: Require a running Redis server

### Environment Variables

To enable Redis integration tests, set the following environment variables:

```bash
# Required - Redis host (enables all Redis integration tests)
export STORAGE_REDIS_HOST=localhost

# Optional - Redis configuration
export STORAGE_REDIS_PORT=6379        # Default: 6379
export STORAGE_REDIS_DB=0             # Default: 0
export STORAGE_REDIS_PASSWORD=mysecret # Default: None
```

### Running Redis Tests

```bash
# Run only unit tests (no Redis server needed)
pytest tests/storage/test_redis_storage.py -v

# Start a Redis server (using Docker)
docker run -d --name test-redis -p 6379:6379 redis:7-alpine

# Run all tests including integration tests
STORAGE_REDIS_HOST=localhost pytest tests/storage/test_redis_storage.py -v

# Run with custom Redis configuration
STORAGE_REDIS_HOST=localhost STORAGE_REDIS_PORT=6380 STORAGE_REDIS_DB=1 STORAGE_REDIS_PASSWORD=secret pytest tests/storage/test_redis_storage.py -v

# Clean up
docker stop test-redis && docker rm test-redis
```

### Redis Test Coverage

Integration tests include:
- Basic put/get/delete operations
- Hierarchical key operations (`batch/job_001` creates Redis sets)
- Timestamp-ordered listing (keys returned in creation order)
- Token-based pagination with Redis ZRANGE
- Hierarchical token pagination

### Test Markers

Integration tests are marked with `@requires_redis` which automatically skips tests if `STORAGE_REDIS_HOST` is not configured.