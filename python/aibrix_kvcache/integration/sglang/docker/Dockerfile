ARG SGLANG_VERSION=latest

FROM docker.io/pytorch/manylinux2_28-builder:cuda12.1 AS builder

ARG AIBRIX_REPO=https://github.com/vllm-project/aibrix
ARG AIBRIX_BRANCH=main
ARG PYTHON_BIN=python
ARG TORCH_VERSION=2.8.0

ENV PATH="/opt/python/cp312-cp312/bin:${PATH}"

# checkout codebase
RUN git clone ${AIBRIX_REPO} /tmp/aibrix && cd /tmp/aibrix && git checkout ${AIBRIX_BRANCH}

# install dependencies
# use the same torch version as sglang
RUN --mount=type=cache,target=/root/.cache/pip ${PYTHON_BIN} -m pip install torch==${TORCH_VERSION}
RUN --mount=type=cache,target=/root/.cache/pip cd /tmp/aibrix && \
    ${PYTHON_BIN} -m pip install -r python/aibrix_kvcache/requirements/build.txt -r python/aibrix_kvcache/requirements/core.txt
    
# build aibrix_kvcache
RUN cd /tmp/aibrix && \
    ${PYTHON_BIN} -m build python/aibrix_kvcache --wheel --outdir=python/aibrix_kvcache/dist --no-isolation
    

FROM lmsysorg/sglang:${SGLANG_VERSION} AS sglang

ARG PYTHON_BIN=python3

COPY --from=builder /tmp/aibrix /tmp/aibrix

RUN ${PYTHON_BIN} -m pip uninstall -y aibrix_kvcache && \
    ${PYTHON_BIN} -m pip install /tmp/aibrix/python/aibrix_kvcache/dist/*.whl
   
RUN rm -rf /tmp/aibrix

WORKDIR /sgl-workspace/sglang
