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

ENV MODEL_NAME=llama2-7b
 # Trigger profiling
RUN python app.py --time_limit 1000

# Expose the port the app runs on
EXPOSE 8000

# Run the application
CMD ["python", "app.py"]
