# KV Event Synchronization Documentation

This directory contains the core documentation for KV Event Synchronization feature in AIBrix.

## Documentation Structure

### Core Documents

1. **[KV Cache Events Guide](./kv-cache-events-guide.md)**
   - Complete feature overview and architecture
   - Configuration and deployment instructions
   - Troubleshooting and best practices
   - Migration guide for existing deployments

2. **[E2E Testing Guide](./testing/e2e-test-guide.md)**
   - Comprehensive testing instructions
   - Test environment setup
   - Running and debugging tests
   - CI/CD integration

### Related Documentation

- **[Testing README](./testing/README.md)** - Overview of the complete testing framework
- **[Package README](../pkg/cache/kvcache/README.md)** - Low-level implementation details

## Quick Links

- **Feature Requirements**: vLLM 0.7.0+, ZMQ support, Remote tokenizer
- **Key Environment Variable**: `AIBRIX_KV_EVENT_SYNC_ENABLED=true`
- **Build Tag**: `-tags="zmq"` (for gateway-plugins only)

## Getting Started

1. Read the [KV Cache Events Guide](./kv-cache-events-guide.md) for feature overview
2. Follow deployment instructions in the guide
3. Use [E2E Testing Guide](./testing/e2e-test-guide.md) for validation

## Support

For issues or questions, please refer to the main AIBrix documentation or create an issue in the repository.