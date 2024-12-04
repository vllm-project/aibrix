# Use the official Python base image
FROM python:3.10-slim

# Set environment variables
ENV PYTHONDONTWRITEBYTECODE=1
ENV PYTHONUNBUFFERED=1
ENV WANDB_MODE=disabled

# Set the working directory
WORKDIR /app

# Copy the requirements file into the container
COPY requirements.txt /app/

# Install dependencies
RUN apt update && apt install -y curl jq git

RUN pip install --no-cache-dir -r requirements.txt

# Copy the rest of the application code into the container
COPY ./*.py /app/

ENV MODEL_NAME=llama2-7b
ARG GPU_TYPE=disabled

# Trigger profiling
RUN if [ "$GPU_TYPE" != "disabled" ]; then \
        python app.py --time_limit 1000 --replica_config_device ${GPU_TYPE}; \
    fi

# Expose the port the app runs on
EXPOSE 8000

# Run the application, environment variable is necessary to apply ARG
ENV GPU_TYPE=$GPU_TYPE
CMD python app.py --replica_config_device ${GPU_TYPE}
