FROM busybox:1.36

ARG INPLACE_E2E_VERSION=unknown
LABEL aibrix.inplace-e2e.version="${INPLACE_E2E_VERSION}"

RUN echo "${INPLACE_E2E_VERSION}" > /inplace-e2e-version
CMD ["sh", "-c", "sleep 3600"]
