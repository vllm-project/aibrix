#!/bin/bash
gunicorn -b :8080 app:app -k uvicorn.workers.UvicornWorker
