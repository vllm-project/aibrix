.PHONY: build
build:
	docker rmi aibrix/tpm:v0.1.0 --force
	docker build -t aibrix/tpm:v0.1.0 -f Dockerfile .
	kind load docker-image aibrix/tpm:v0.1.0

.PHONY: apply
apply:
	kubectl apply -f deployment.yaml
	kubectl apply -f plugins.yaml

.PHONY: delete
delete:
	kubectl delete -f deployment.yaml 
	kubectl delete -f plugins.yaml