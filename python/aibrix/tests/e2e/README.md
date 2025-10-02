# End-to-End Tests

This directory contains end-to-end tests for Aibrix services that run against real service instances.

## Files

- `test_batch_api.py` - E2E tests for OpenAI Batch API endpoints

## Prerequisites

1. **Running Aibrix Service**: Ensure the Aibrix service is accessible at `http://localhost:8888/`
   ```bash
   # Example: Access the service using port-forward
   kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80
   ```

2. **Generate Credentials**: Ensure object store is acceesible. Using S3 as an example:
   ```bash
   python ../../scripts/generate_secrets.py s3 --bucket <your-bucket-name>
   ```
   The script will read s3 credentials setup using ```aws configure```

## Running Tests

### All E2E Tests
```bash
cd /path/to/aibrix/python/aibrix
pytest tests/e2e/ -v
```

### Batch API Tests Only
```bash
cd /path/to/aibrix/python/aibrix
pytest tests/e2e/test_batch_api.py -v
```

### Specific Test
```bash
cd /path/to/aibrix/python/aibrix
pytest tests/e2e/test_batch_api.py::test_batch_api_e2e_real_service -v
```

### With Detailed Output
```bash
cd /path/to/aibrix/python/aibrix
pytest tests/e2e/test_batch_api.py -v -s
```

## Test Structure

### Service Health Fixture
- `service_health` - Session-scoped fixture that checks service availability
- Tests `/healthz` endpoint for basic health checking
- Automatically skips all tests if service is not available

### Service Availability Test
- `test_batch_api_service_availability()` - Verifies service is running and accessible
- Tests basic API endpoint accessibility

### Complete Workflow Test  
- `test_batch_api_e2e_real_service()` - Tests the complete batch processing workflow
- Upload → Create → Poll → Download → Verify

### Error Handling Test
- `test_batch_api_error_handling_real_service()` - Tests error scenarios
- Invalid inputs, non-existent resources

## API Endpoints

### Health Endpoints
- `/healthz` - General service health check

## Configuration

Tests connect to `http://localhost:8888` by default. The service URL is hardcoded in the test functions.

## Expected Output

Successful test run:
```
tests/e2e/test_batch_api.py::test_batch_api_service_availability PASSED
tests/e2e/test_batch_api.py::test_batch_api_e2e_real_service PASSED  
tests/e2e/test_batch_api.py::test_batch_api_error_handling_real_service PASSED
```

If service is not available:
```
tests/e2e/test_batch_api.py::test_batch_api_service_availability SKIPPED
tests/e2e/test_batch_api.py::test_batch_api_e2e_real_service SKIPPED
tests/e2e/test_batch_api.py::test_batch_api_error_handling_real_service SKIPPED
```