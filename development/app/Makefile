# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Image URL to use all building/pushing image targets
AIBRIX_CONTAINER_REGISTRY_NAMESPACE ?= aibrix

docker-build-all: docker-build-mock docker-build-simulator docker-build-simulator-a40

docker-build-mock:
	$(CONTAINER_TOOL) build -t ${AIBRIX_CONTAINER_REGISTRY_NAMESPACE}/vllm-mock:nightly -f Dockerfile .

docker-build-simulator: 
	$(CONTAINER_TOOL) build -t ${AIBRIX_CONTAINER_REGISTRY_NAMESPACE}/vllm-simulator:nightly --build-arg SIMULATION=a100 -f Dockerfile .

docker-build-simulator-a40:
	$(CONTAINER_TOOL) build -t ${AIBRIX_CONTAINER_REGISTRY_NAMESPACE}/vllm-simulator-a40:nightly --build-arg SIMULATION=a40 -f Dockerfile .

docker-build: docker-build-mock

deploy-mock:
	kubectl apply -k config/mock
	sleep 2
	kubectl port-forward svc/llama2-7b 8000:8000 1>/dev/null 2>&1 &
	kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 1>/dev/null 2>&1 &

deploy-simulator:
	kubectl apply -k config/simulator
	sleep 2
	kubectl port-forward svc/llama2-7b 8000:8000 1>/dev/null 2>&1 &
	kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 1>/dev/null 2>&1 &

deploy-heterogeneous:
	kubectl apply -k config/heterogeneous
	sleep 2
	kubectl port-forward svc/llama2-7b 8000:8000 1>/dev/null 2>&1 &
	kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 1>/dev/null 2>&1 &

deploy: deploy-mock

clean-mock:
	kubectl delete -k config/mock
	sleep 1
	curl http://localhost:8000/metrics
	curl http://localhost:8888/metrics

clean-simulator:
	kubectl delete -k config/simulator
	sleep 1
	curl http://localhost:8000/metrics
	curl http://localhost:8888/metrics

clean-heterogeneous:
	kubectl delete -k config/heterogeneous
	sleep 1
	curl http://localhost:8000/metrics
	curl http://localhost:8888/metrics

clean: clean-mock

test:
	curl http://localhost:8000/v1/chat/completions \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key" \
		-d '{ \
			"model": "llama2-7b", \
			"messages": [{"role": "user", "content": "Say this is a test!"}], \
			"temperature": 0.7 \
		}'

test2:
	curl http://localhost:8000/v1/completions \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key" \
		-d '{ \
			"model": "llama2-7b", \
			"prompt": "Say this is a test!", \
			"temperature": 0.7, \
			"max_tokens": 50 \
		}'

test-long:
	curl http://localhost:8000/v1/completions \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key" \
		-d '{ \
			"model": "llama2-7b", \
			"prompt": "Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test! Say this is a test!", \
			"temperature": 0.7, \
			"max_tokens": 50 \
		}'

test-gateway:
	curl -v http://localhost:8888/v1/chat/completions \
		-H "model: llama2-7b" \
		-H "Content-Type: application/json" \
		-H "Authorization: Bearer any_key" \
		-d '{ \
			"model": "llama2-7b", \
			"messages": [{"role": "user", "content": "Say this is a test!"}], \
			"temperature": 0.7 \
		}'

test-gateway2:
	curl -v http://localhost:8888/v1/completions \
		-H "Content-Type: application/json" \
		-d '{ \
            "model": "llama2-7b", \
            "prompt": "Say this is a test!", \
            "temperature": 0.0, \
            "max_tokens": 512 \
        }'

metrics:
	curl http://localhost:8000/metrics