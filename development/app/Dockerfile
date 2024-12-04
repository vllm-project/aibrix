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
ENV HUGGINGFACE_TOKEN="your huggingface token"
ARG SIMULATION=disabled

# Trigger profiling
RUN if [ "$SIMULATION" != "disabled" ]; then \
        python app.py --time_limit 1000 --replica_config_device ${SIMULATION}; \
    fi

# Expose the port the app runs on
EXPOSE 8000

# Run the application, environment variable is necessary to apply ARG
ENV SIMULATION=$SIMULATION
CMD python app.py --replica_config_device ${SIMULATION}
