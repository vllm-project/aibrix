ARG VLLM_VERSION=v0.10.2

# Extract torch version from the vLLM base image
FROM vllm/vllm-openai:${VLLM_VERSION} AS torch-version
ARG PYTHON_BIN=python3
RUN ${PYTHON_BIN} -m pip show torch | awk '/Version/{print $2}' | sed 's/+.*$//' > /torch_version.txt

# Builder stage
FROM docker.io/pytorch/manylinux2_28-builder:cuda12.1 AS builder

ARG AIBRIX_REPO=https://github.com/vllm-project/aibrix
ARG AIBRIX_BRANCH=main
ARG PYTHON_BIN=python

ENV PATH="/opt/python/cp312-cp312/bin:${PATH}"

# checkout codebase
RUN git clone ${AIBRIX_REPO} /tmp/aibrix && cd /tmp/aibrix && git checkout ${AIBRIX_BRANCH}
    
# install dependencies
# use the same torch version as vllm
COPY --from=torch-version /torch_version.txt /tmp/torch_version.txt
RUN --mount=type=cache,target=/root/.cache/pip ${PYTHON_BIN} -m pip install torch==$(cat /tmp/torch_version.txt) && rm /tmp/torch_version.txt
RUN --mount=type=cache,target=/root/.cache/pip cd /tmp/aibrix && \
    ${PYTHON_BIN} -m pip install -r python/aibrix_kvcache/requirements/build.txt -r python/aibrix_kvcache/requirements/core.txt
    
# build aibrix_kvcache
RUN cd /tmp/aibrix && \
    ${PYTHON_BIN} -m build python/aibrix_kvcache --wheel --outdir=python/aibrix_kvcache/dist --no-isolation
    
# Runtime stage
FROM vllm/vllm-openai:${VLLM_VERSION} AS vllm-openai

ARG PYTHON_BIN=python3
ARG VLLM_VERSION=v0.10.2

COPY --from=builder /tmp/aibrix /tmp/aibrix

RUN ${PYTHON_BIN} -m pip uninstall -y aibrix_kvcache && \
    ${PYTHON_BIN} -m pip install /tmp/aibrix/python/aibrix_kvcache/dist/*.whl

# apply patch to vLLM
RUN DIST_DIR=$(${PYTHON_BIN} -m pip show vllm | grep "Location:" | awk '{print $2}') && \
    cd $DIST_DIR && \
    patch -p 1 -l -i /tmp/aibrix/python/aibrix_kvcache/integration/vllm/patches/vllm_${VLLM_VERSION}-aibrix-kvcache.patch
    
RUN rm -rf /tmp/aibrix

ENTRYPOINT ["python3", "-m", "vllm.entrypoints.openai.api_server"]
