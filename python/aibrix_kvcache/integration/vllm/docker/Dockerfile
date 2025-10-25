ARG VLLM_VERSION=v0.10.2

FROM docker.io/pytorch/manylinux2_28-builder:cuda12.1 AS builder

ARG AIBRIX_REPO=https://github.com/vllm-project/aibrix
ARG AIBRIX_BRANCH=main
ARG PYTHON_BIN=python
ARG TORCH_VERSION=2.8.0

ENV PATH="/opt/python/cp312-cp312/bin:${PATH}"

# checkout codebase
RUN git clone --branch ${AIBRIX_BRANCH} --depth 1 ${AIBRIX_REPO} /tmp/aibrix
    
# install dependencies
# use the same torch version as vllm
RUN --mount=type=cache,target=/root/.cache/pip ${PYTHON_BIN} -m pip install torch==${TORCH_VERSION}
RUN --mount=type=cache,target=/root/.cache/pip cd /tmp/aibrix && \
    ${PYTHON_BIN} -m pip install -r python/aibrix_kvcache/requirements/build.txt -r python/aibrix_kvcache/requirements/core.txt
    
# build aibrix_kvcache
RUN cd /tmp/aibrix && \
    ${PYTHON_BIN} -m build python/aibrix_kvcache --wheel --outdir=python/aibrix_kvcache/dist --no-isolation
    

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
