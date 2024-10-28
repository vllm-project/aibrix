all: build

build: 
	docker build -t aibrix/gpu-optimizer:nightly -f Dockerfile .

deploy:
	kubectl apply -f deployment.yaml
	sleep 2
	kubectl -n aibrix-system port-forward svc/gpu-optimizer 8080:8080 1>/dev/null 2>&1 &

clean:
	kubectl delete -f deployment.yaml
	sleep 1
	curl http://localhost:8080/metrics/aibrix-system/simulator-llama2-7b

debug-init-simulator:
	curl http://localhost:8080/monitor/aibrix-system/simulator-llama2-7b \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key" \
		-d '{}'
		
debug-scale-simulator:
	curl http://localhost:8080/scale/aibrix-system/simulator-llama2-7b/2 \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key" \
		-d '{}'

debug-metrics:
	curl http://localhost:8080/metrics/aibrix-system/simulator-llama2-7b