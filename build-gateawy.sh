#!/bin/bash

tag=gangmuk-test
# build
make docker-build-gateway-plugins
# tag
docker tag aibrix/gateway-plugins:nightly aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/gateway-plugins:$tag
# push
docker push aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/gateway-plugins:$tag