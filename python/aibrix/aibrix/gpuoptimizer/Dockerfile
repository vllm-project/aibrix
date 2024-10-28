# Use the official Python base image
FROM python:3.10-slim

# Set environment variables

# Set the working directory
WORKDIR /optimizer

# Copy the requirements file into the container
COPY requirements.txt /optimizer/

# Install dependencies
RUN apt update && apt install -y curl jq git gcc

RUN pip install --no-cache-dir -r requirements.txt

# Copy the rest of the application code into the container
COPY ./*.py /optimizer/
COPY ./loadmonitor /optimizer/loadmonitor/
COPY ./optimizer /optimizer/optimizer/


# Expose the port the app runs on
EXPOSE 8080

# Run the application
CMD ["python", "app.py"]
