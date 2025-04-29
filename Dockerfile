FROM alpine:latest
RUN echo "Hello from $(uname -m)" > /hello.txt
CMD ["cat", "/hello.txt"]