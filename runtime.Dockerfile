ARG PYTHON_VERSION=3.11
ARG BASE_IMAGE=python:${PYTHON_VERSION}-slim-bookworm

FROM ${BASE_IMAGE} AS base

WORKDIR /app

# Install Poetry
ARG POETRY_VERSION=1.8.3
RUN python3 -m pip install poetry==${POETRY_VERSION}

# Copy the runtime source
COPY python/aibrix/poetry.lock python/aibrix/pyproject.toml python/aibrix/ /app/

# Install dependencies
RUN poetry config virtualenvs.create false \
    && poetry install --no-root \
    && poetry cache clear pypi --all

# Set entrypoint for Runtime
COPY python/aibrix/scripts/entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh
ENTRYPOINT ["/app/entrypoint.sh"]

