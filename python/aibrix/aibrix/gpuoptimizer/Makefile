all: build

DP ?= "profiling"

build: 
	docker build -t aibrix/gpu-optimizer:nightly -f Dockerfile .

.PHONY: deploy
deploy:
	kubectl apply -f deployment.yaml
	sleep 2
	kubectl -n aibrix-system port-forward svc/gpu-optimizer 8080:8080 1>/dev/null 2>&1 &

.PHONY: clean
clean:
	kubectl delete -f deployment.yaml
	sleep 1
	curl http://localhost:8080/metrics/aibrix-system/simulator-llama2-7b

.PHONY: benchmark
benchmark:
	optimizer/profiling/benchmark.sh $(DP)

.PHONY: gen-profile
gen-profile:
	python optimizer/profiling/gen-profile.py --benchmark optimizer/profiling/result/$(DP).jsonl -o optimizer/profiling/result/$(DP).json

.PHONY: debug-init
debug-init:
	kubectl -n aibrix-system port-forward svc/aibrix-redis-master 6379:6379 1>/dev/null 2>&1 &

.PHONY: debug
debug:
	python -m app --debug

.PHONY: debug-init-simulator
debug-init-simulator:
	curl http://localhost:8080/monitor/aibrix-system/simulator-llama2-7b \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key" \
		-d '{}'

.PHONY: debug-scale-simulator	
debug-scale-simulator:
	curl http://localhost:8080/scale/aibrix-system/simulator-llama2-7b/2 \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key" \
		-d '{}'

.PHONY: debug-stop-simulator
debug-stop-simulator:
	curl -X DELETE http://localhost:8080/monitor/aibrix-system/simulator-llama2-7b \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key"

.PHONY: debug-metrics
debug-metrics:
	curl http://localhost:8080/metrics/aibrix-system/simulator-llama2-7b

.PHONY: debug-workload
debug-workload:
	python optimizer/profiling/gpu-benchmark.py --backend=vllm --port 8000 --request-rate=10 --num-prompts=1 --input_len 4000 --output_len 512 --model=llama2-7b

.PHONY: visualizer
visualizer:
	python -m loadmonitor.visualizer --dataset /Users/bytedance/Studio/gpu/datasets/bird/token_counts.csv --profile optimizer/profiling/result/$(DP).json