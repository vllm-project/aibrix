# GPU Optimizer: a vLLM Auto Scaler with Heterogeneous GPU support

## Run in kubernetes

1. Make sure Aibrix components are up-to-date.

2. For now, build GPU Optimizer base image using Dockerfile within this folder.
```dockerfile
docker build -t aibrix/gpu-optimizer:nightly -f Dockerfile .

# Or use make
make build
```

3. Prepare performance benchmark using optimizer/profiling/benchmark.sh. See optimizer/profiling/README.md. You may need to expose pod interface first:
```shell
# Make sure pod is accessable locally:
kubectl -n aibrix-system port-forward [pod_name] 8010:8000 1>/dev/null 2>&1 &
```

If using CPU based vLLM simulator, sample profiles is included in optimizer/profiling/result.
	

1. Generate profile based on SLO target using optimizer/profiling/gen-profile.py. If using CPU based vLLM simulator, execute
```shell
# Make sure Redis is accessable locally:
kubectl -n aibrix-system port-forward svc/aibrix-redis-master 6379:6379 1>/dev/null 2>&1 &
# Or use make
make debug-init

python optimizer/profiling/gen-profile.py simulator-llama2-7b-a100 -o "redis://localhost:6379/?model=llama2-7b"
# Or use make
make DP=simulator-llama2-7b-a100 gen-profile
```

5. Deploy GPU Optimizer
```shell
kubectl apply -f deployment.yaml
kubectl -n aibrix-system port-forward svc/gpu-optimizer 8080:8080 1>/dev/null 2>&1 &

# Or use make
make deploy
```

4. Deploy your vLLM model. If run locally a CPU based vLLM simulator is provided. See docs/development/simulator for details

5. Start workload and see how model scale. Benchmark toolkit can be used to generate workload as:
```shell
# Make sure gateway's local access, see docs/development/simulator/README.md for details.
python optimizer/profiling/gpu-benchmark.py --backend=vllm --port 8888 --request-rate=10 --num-prompts=100 --input_len 2000 --output_len 128 --model=llama2-7b
```

6. Observability: visit http://localhost:8080/dash/llama2-7b for workload pattern visualization. A independent visualization demo can access by:
```
python -m loadmonitor.visualizer
```