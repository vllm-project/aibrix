all: build

DP ?= profiling
DATASET ?= [set your DATASET path]

.PHONY: benchmark
benchmark:
	optimizer/profiling/benchmark.sh $(DP)

.PHONY: gen-profile
gen-profile:
	python optimizer/profiling/gen_profile.py $(DP) -o "redis://localhost:6379/?model=llama2-7b"

.PHONY: debug-init
debug-init:
	kubectl -n aibrix-system port-forward svc/aibrix-gpu-optimizer 8080:8080 1>/dev/null 2>&1 &
	kubectl -n aibrix-system port-forward svc/aibrix-redis-master 6379:6379 1>/dev/null 2>&1 &

.PHONY: debug
debug:
	python -m app --debug

.PHONY: debug-init-simulator
debug-init-simulator:
	curl http://localhost:8080/monitor/default/simulator-llama2-7b-a100 \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key" \
		-d '{}'

.PHONY: debug-scale-simulator	
debug-scale-simulator:
	curl -X PUT http://localhost:8080/scale/default/simulator-llama2-7b-a100\
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key" \
		-d '{"replicas":"0"}'

.PHONY: debug-stop-simulator
debug-stop-simulator:
	curl -X DELETE http://localhost:8080/monitor/default/simulator-llama2-7b-a100 \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key"

.PHONY: debug-update-profile
debug-update-profile:
	curl http://localhost:8080/update_profile/llama2-7b

.PHONY: debug-metrics
debug-metrics:
	curl http://localhost:8080/metrics/default/simulator-llama2-7b-a100

.PHONY: debug-workload
debug-workload:
	python optimizer/profiling/gpu_benchmark.py --backend=vllm --port 8888 --request-rate=10 --num-prompts=100 --input_len 2000 --output_len 512 --model=llama2-7b --verbose

.PHONY: visualizer
visualizer:
	python -m load_monitor.visualizer --dataset $(DATASET) --redisprofile "redis://localhost:6379/?model=llama2-7b"