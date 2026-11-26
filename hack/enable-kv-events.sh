#!/bin/bash
# migration/enable-kv-events.sh

# Script to enable KV events for existing deployments

NAMESPACE=${1:-default}
DEPLOYMENT=$2
ENGINE=${3:-vllm}
DP_SIZE=${4:-1}

if [ -z "$DEPLOYMENT" ] || [[ ! "$ENGINE" =~ ^(vllm|sglang)$ ]] || [[ ! "$DP_SIZE" =~ ^[1-9][0-9]*$ ]]; then
    echo "Usage: $0 [namespace] <deployment-name> [vllm | sglang [dp-size]]"
    exit 1
fi

echo "Enabling KV events for deployment $DEPLOYMENT in namespace $NAMESPACE"

# Add label to deployment
kubectl label deployment -n $NAMESPACE $DEPLOYMENT \
    model.aibrix.ai/kv-events-enabled=true --overwrite

if [ "$ENGINE" = "sglang" ]; then
    # Each SGLang DP rank binds the event and replay ports plus its rank, so
    # the replay range starts after the DP_SIZE event ports.
    REPLAY_PORT=$((5557 + DP_SIZE))
    kubectl patch deployment -n "$NAMESPACE" "$DEPLOYMENT" --type='json' -p='[
      {
        "op": "add",
        "path": "/spec/template/metadata/labels/model.aibrix.ai~1kv-events-enabled",
        "value": "true"
      },
      {
        "op": "add",
        "path": "/spec/template/spec/containers/0/ports/-",
        "value": {
          "containerPort": 5557,
          "protocol": "TCP",
          "name": "kv-events"
        }
      },
      {
        "op": "add",
        "path": "/spec/template/spec/containers/0/ports/-",
        "value": {
          "containerPort": '"$REPLAY_PORT"',
          "protocol": "TCP",
          "name": "kv-replay"
        }
      }
    ]'
    echo "SGLang reads these settings from its command line. Add these flags to the launch command:"
    echo "  --kv-events-config '{\"publisher\":\"zmq\",\"endpoint\":\"tcp://*:5557\",\"replay_endpoint\":\"tcp://*:$REPLAY_PORT\"}' --page-size 16"
    exit 0
fi

# Patch deployment to add KV event configuration
kubectl patch deployment -n $NAMESPACE $DEPLOYMENT --type='json' -p='[
  {
    "op": "add",
    "path": "/spec/template/metadata/labels/model.aibrix.ai~1kv-events-enabled",
    "value": "true"
  },
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/env/-",
    "value": {
      "name": "VLLM_ENABLE_KV_CACHE_EVENTS",
      "value": "true"
    }
  },
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/env/-",
    "value": {
      "name": "VLLM_KV_EVENTS_PUBLISHER",
      "value": "zmq"
    }
  },
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/env/-",
    "value": {
      "name": "VLLM_KV_EVENTS_ENDPOINT",
      "value": "tcp://*:5557"
    }
  },
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/env/-",
    "value": {
      "name": "VLLM_KV_EVENTS_REPLAY_ENDPOINT",
      "value": "tcp://*:5558"
    }
  },
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/ports/-",
    "value": {
      "containerPort": 5557,
      "protocol": "TCP",
      "name": "kv-events"
    }
  },
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/ports/-",
    "value": {
      "containerPort": 5558,
      "protocol": "TCP",
      "name": "kv-replay"
    }
  }
]'

echo "Deployment updated. Pods will be recreated with KV events enabled."