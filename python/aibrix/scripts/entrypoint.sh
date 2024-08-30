#!/bin/bash
gunicorn -b :8000 app:app -k uvicorn.workers.UvicornWorker
