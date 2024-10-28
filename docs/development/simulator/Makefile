all: build

build: 
	docker build -t aibrix/vllm-simulator:nightly -f Dockerfile .

deploy:
	kubectl apply -f deployment.yaml
	sleep 2
	kubectl -n aibrix-system port-forward svc/llama2-7b 8000:8000 1>/dev/null 2>&1 &

clean:
	kubectl delete -f deployment.yaml
	sleep 1
	curl http://localhost:8000/metrics

test:
	curl http://localhost:8000/v1/chat/completions \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key" \
		-d '{
			"model": "llama2-7b",
			"messages": [{"role": "user", "content": "Say this is a test!"}],
			"temperature": 0.7
		}'

metrics:
	curl http://localhost:8000/metrics