FROM python:3.12-alpine
WORKDIR /app
COPY router.py /app/router.py
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["python", "/app/router.py"]

