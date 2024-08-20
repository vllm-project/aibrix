## Multistage build
FROM golang:1.22 as build
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64

WORKDIR /src
COPY . .
RUN go mod download
RUN go build -o /ext_proc

## Multistage deploy
FROM gcr.io/distroless/base-debian10

WORKDIR /
COPY --from=build /ext_proc /ext_proc

ENTRYPOINT ["/ext_proc"]
