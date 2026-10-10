FROM golang:1.24-alpine AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -o /external-router main.go
FROM scratch
COPY --from=build /external-router /external-router
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/external-router"]

