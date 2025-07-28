# Storm Service Upgrade Orchestration

This document describes the advanced upgrade orchestration capabilities added to Storm Service, providing controlled deployment strategies similar to Argo Rollouts.

## Overview

Storm Service now supports sophisticated upgrade orchestration capabilities that enable risk-aware deployment strategies including:

- **Canary Releases**: Gradual traffic shifting with automated or manual promotion
- **Blue-Green Deployments**: Full environment switching with zero-downtime
- **Automated Rollbacks**: Health-check based automatic rollback mechanisms
- **Manual Controls**: Pause, resume, abort, and approve rollouts
- **Analysis Integration**: Automated analysis and validation during rollouts

## Features

### 1. Canary Deployment Strategy

Canary deployments allow you to gradually roll out changes by routing a percentage of traffic to the new version while monitoring metrics and health.

#### Configuration Example

```yaml
apiVersion: orchestration.aibrix.ai/v1alpha1
kind: StormService
metadata:
  name: my-app
spec:
  rolloutStrategy:
    type: Canary
    canary:
      steps:
      - weight: 10    # Route 10% traffic to canary
        pause:
          duration: 5m
      - weight: 25    # Route 25% traffic to canary
        pause:
          untilApproval: true  # Wait for manual approval
        analysis:
          templates:
          - name: success-rate-check
      - weight: 50    # Route 50% traffic to canary
      - weight: 100   # Full promotion
      
      trafficRouting:
        nginx:
          ingress: my-app-ingress
          servicePort: 80
      
      stableService: my-app-stable
      canaryService: my-app-canary
```

#### Step Types

- **Weight-based**: Route a percentage of traffic to the canary
- **Replica-based**: Scale canary to a specific number of replicas
- **Pause**: Wait for a duration or manual approval
- **Analysis**: Run automated validation checks

### 2. Blue-Green Deployment Strategy

Blue-Green deployments create a complete parallel environment (green) while the current environment (blue) serves production traffic.

#### Configuration Example

```yaml
apiVersion: orchestration.aibrix.ai/v1alpha1
kind: StormService
metadata:
  name: my-app
spec:
  rolloutStrategy:
    type: BlueGreen
    blueGreen:
      activeService: my-app-active
      previewService: my-app-preview
      autoPromotionEnabled: false
      scaleDownDelaySeconds: 600
      
      prePromotionAnalysis:
        templates:
        - name: integration-tests
        - name: smoke-tests
      
      postPromotionAnalysis:
        templates:
        - name: monitoring-check
```

### 3. Automatic Rollback

Configure automatic rollback triggers based on health checks, analysis failures, or timeouts.

```yaml
autoRollback:
  enabled: true
  onFailure:
    analysisFailure: true
    healthCheckFailure: true
    errorThreshold: 5%
  onTimeout: 30m
```

### 4. Traffic Routing

Support for multiple traffic routing mechanisms:

#### Nginx Ingress
```yaml
trafficRouting:
  nginx:
    ingress: my-app-ingress
    servicePort: 80
```

#### Istio Service Mesh
```yaml
trafficRouting:
  istio:
    virtualService: my-app-vs
    destinationRule: my-app-dr
```

#### SMI (Service Mesh Interface)
```yaml
trafficRouting:
  smi:
    trafficSplit: my-app-split
```

## Manual Rollout Controls

You can control rollouts manually using annotations:

### Pause a Rollout
```bash
kubectl annotate stormservice my-app stormservice.aibrix.ai/rollout-pause=true
```

### Resume a Paused Rollout
```bash
kubectl annotate stormservice my-app stormservice.aibrix.ai/rollout-resume=true
```

### Approve a Rollout Waiting for Manual Approval
```bash
kubectl annotate stormservice my-app stormservice.aibrix.ai/rollout-approval=true
```

### Abort a Rollout and Trigger Rollback
```bash
kubectl annotate stormservice my-app stormservice.aibrix.ai/rollout-abort=true
```

### Retry a Failed Rollout
```bash
kubectl annotate stormservice my-app stormservice.aibrix.ai/rollout-retry=true
```

## Monitoring Rollout Status

### Check Rollout Status
```bash
kubectl get stormservice my-app -o jsonpath='{.status.rolloutStatus}'
```

### Watch Rollout Progress
```bash
kubectl get stormservice my-app -w
```

### Detailed Status Information
```bash
kubectl describe stormservice my-app
```

### Rollout Status Fields

The rollout status provides detailed information about the current state:

```yaml
status:
  rolloutStatus:
    phase: Progressing
    currentStepIndex: 1
    currentStepHash: abc12345
    pauseStartTime: "2025-01-20T10:30:00Z"
    conditions:
    - type: Progressing
      status: "True"
      reason: StepComplete
      message: "Step 0 completed successfully"
    canaryStatus:
      currentWeight: 25
      stableRS: my-app-abc123
      canaryRS: my-app-def456
    message: "Canary rollout in progress"
```

## Rollout Phases

- **Healthy**: Rollout is stable and healthy
- **Progressing**: Rollout is actively progressing through steps
- **Paused**: Rollout is paused (manually or automatically)
- **Completed**: Rollout has completed successfully
- **Degraded**: Rollout has encountered issues
- **Aborted**: Rollout was manually aborted

## Analysis and Health Checks

### Analysis Templates

Analysis templates define how to validate the health of your deployment:

```yaml
analysis:
  templates:
  - name: success-rate
  - name: latency-check
  args:
  - name: service-name
    value: my-app
  - name: threshold
    value: "99%"
```

### Health Check Integration

The system automatically performs health checks during rollouts:

- **Replica Health**: Monitors ready vs. total replicas
- **Error Rate Monitoring**: Tracks error rates and success rates
- **Custom Metrics**: Integration with monitoring systems
- **Timeout Detection**: Automatic timeout handling

### Auto-Rollback Triggers

Rollbacks can be triggered automatically by:

- Analysis template failures
- Health check failures
- Error rate exceeding thresholds
- Rollout timeouts
- Manual abort commands

## Integration Points

### Metrics and Monitoring

The rollout system integrates with:

- **Prometheus**: For metrics collection and analysis
- **Datadog**: For APM and monitoring
- **Custom metrics providers**: Via analysis templates

### Traffic Management

Supports multiple traffic management solutions:

- **Nginx Ingress Controller**: Canary annotations
- **Istio Service Mesh**: VirtualService and DestinationRule
- **SMI**: TrafficSplit resources
- **Service-based routing**: Default Kubernetes services

### CI/CD Integration

The annotation-based control system enables easy integration with CI/CD pipelines:

```bash
# In your deployment pipeline
kubectl set image stormservice/my-app app=my-app:v2.0
kubectl annotate stormservice my-app stormservice.aibrix.ai/rollout-resume=true

# Wait for completion or abort on failure
```

## Best Practices

### 1. Start with Conservative Steps
```yaml
steps:
- weight: 5     # Start small
  pause:
    duration: 5m
- weight: 15
  analysis:
    templates:
    - name: basic-health-check
- weight: 30
  pause:
    untilApproval: true  # Manual gate
```

### 2. Use Analysis at Critical Points
```yaml
steps:
- weight: 25
  analysis:
    templates:
    - name: error-rate
    - name: latency-p95
    - name: business-metrics
```

### 3. Configure Appropriate Timeouts
```yaml
autoRollback:
  enabled: true
  onTimeout: 30m  # Adjust based on your deployment time
```

### 4. Monitor Key Metrics
- Success rate (>99%)
- Error rate (<1%)
- Response time (p95 < 200ms)
- Business-specific metrics

### 5. Test Rollback Procedures
Regularly test your rollback mechanisms:
```bash
# Test abort functionality
kubectl annotate stormservice my-app stormservice.aibrix.ai/rollout-abort=true
```

## Migration Guide

### From Standard Rolling Updates

1. **Add rollout strategy** to your StormService spec
2. **Configure traffic routing** if using ingress or service mesh
3. **Set up analysis templates** for your metrics
4. **Test with low-risk deployments** first

### Example Migration

Before:
```yaml
spec:
  updateStrategy:
    type: RollingUpdate
    maxUnavailable: 1
    maxSurge: 25%
```

After:
```yaml
spec:
  updateStrategy:
    type: RollingUpdate
    maxUnavailable: 1
    maxSurge: 25%
  rolloutStrategy:
    type: Canary
    canary:
      steps:
      - weight: 20
        pause:
          duration: 2m
      - weight: 50
        pause:
          untilApproval: true
      autoRollback:
        enabled: true
        onFailure:
          healthCheckFailure: true
```

## Troubleshooting

### Common Issues

1. **Rollout Stuck in Paused State**
   - Check if manual approval is required
   - Verify pause duration hasn't expired
   - Look for analysis failures

2. **Traffic Not Routing Correctly**
   - Verify ingress/service mesh configuration
   - Check service selectors
   - Validate traffic routing setup

3. **Health Checks Failing**
   - Review replica readiness
   - Check application startup time
   - Verify health check endpoints

### Debug Commands

```bash
# Check rollout events
kubectl get events --field-selector involvedObject.name=my-app

# View detailed rollout status
kubectl get stormservice my-app -o yaml

# Check role set status
kubectl get rolesets -l stormservice.aibrix.ai/name=my-app
```

## Conclusion

The Storm Service upgrade orchestration capabilities provide enterprise-grade deployment strategies with fine-grained control over the rollout process. By leveraging canary deployments, blue-green strategies, and automated rollback mechanisms, you can significantly reduce the risk of production deployments while maintaining high availability and rapid iteration capabilities.
