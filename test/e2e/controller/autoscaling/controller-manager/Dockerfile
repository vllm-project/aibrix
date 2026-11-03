FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY bin/controller-manager-e2e manager
USER 65532:65532

ENTRYPOINT ["/manager"]
