# Use the official Python base image
FROM python:3.10-slim

# Set environment variables
ENV PYTHONDONTWRITEBYTECODE=1
ENV PYTHONUNBUFFERED=1
ENV WANDB_MODE=disabled

# Set the working directory
WORKDIR /simulator

# Copy the requirements file into the container
COPY requirements.txt /simulator/

# Install dependencies
RUN apt update && apt install -y curl jq git

RUN pip install --no-cache-dir -r requirements.txt

# Copy the rest of the application code into the container
COPY ./*.py /simulator/
# COPY ./model_cache /simulator/model_cache

ENV MODEL_NAME=llama2-7b
ARG GPU_TYPE=a100
 # Trigger profiling
RUN python app.py --time_limit 1000 --replica_config_device ${GPU_TYPE}

# Expose the port the app runs on
EXPOSE 8000

# Run the application
CMD ["python", "app.py"]
