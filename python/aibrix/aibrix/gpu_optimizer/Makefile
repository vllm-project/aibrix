all: build

DP ?= profiling
DATASET ?= [set your DATASET path]

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
	python optimizer/profiling/gen_profile.py $(DP) -o "redis://localhost:6379/?model=llama2-7b"

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
	python optimizer/profiling/gpu_benchmark.py --backend=vllm --port 8888 --request-rate=10 --num-prompts=100 --input_len 2000 --output_len 512 --model=llama2-7b --verbose

.PHONY: visualizer
visualizer:
	python -m load_monitor.visualizer --dataset $(DATASET) --redisprofile "redis://localhost:6379/?model=llama2-7b"