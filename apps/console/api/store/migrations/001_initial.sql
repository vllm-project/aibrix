-- 001_initial.sql
-- Initial MySQL schema for the AIBrix Console.
-- Tables correspond to the protobuf messages defined in console/v1/console.proto
-- and the Store interface in store.go.

-- ---------------------------------------------------------------------------
-- Deployments
-- Core fields from the Deployment protobuf message plus StormService extension
-- fields used when reconciling with the Kubernetes backend.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS deployments (
    id                VARCHAR(36)   NOT NULL PRIMARY KEY,
    name              VARCHAR(255)  NOT NULL,
    deployment_id     VARCHAR(255)  NOT NULL,
    base_model        VARCHAR(255)  NOT NULL DEFAULT '',
    base_model_id     VARCHAR(255)  NOT NULL DEFAULT '',
    replicas          VARCHAR(255)  NOT NULL DEFAULT '1',
    gpus_per_replica  INT           NOT NULL DEFAULT 0,
    gpu_type          VARCHAR(255)  NOT NULL DEFAULT '',
    region            VARCHAR(255)  NOT NULL DEFAULT '',
    created_by        VARCHAR(255)  NOT NULL DEFAULT '',
    status            VARCHAR(255)  NOT NULL DEFAULT 'Deploying',

    -- StormService extension fields
    model_source      VARCHAR(255)  NOT NULL DEFAULT '',
    model_artifact_url VARCHAR(1000) NOT NULL DEFAULT '',
    engine            VARCHAR(255)  NOT NULL DEFAULT '',
    startup_command   TEXT,
    env_vars          JSON,
    extra_args        JSON,
    namespace         VARCHAR(255)  NOT NULL DEFAULT 'default',
    k8s_resource_name VARCHAR(255)  NOT NULL DEFAULT '',

    created_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    INDEX idx_deployments_name (name),
    INDEX idx_deployments_status (status),
    INDEX idx_deployments_created_by (created_by)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Jobs (Batch Inference)
--
-- The Console persists only the product-side fields it owns (id, display
-- name, created_by, future: organization, tags, ...). OpenAI Batch state
-- (status, usage, request_counts, timestamps, ...) lives in the metadata
-- service and is fetched at request time by the JobHandler, then merged
-- with this row to produce the wire-level Job.
--
-- The id column equals the metadata service's batch id (e.g. "batch_xxx").
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS jobs (
    id                VARCHAR(64)   NOT NULL PRIMARY KEY,
    name              VARCHAR(255)  NOT NULL DEFAULT '',
    created_by        VARCHAR(255)  NOT NULL DEFAULT '',

    created_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    INDEX idx_jobs_created_by (created_by)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Models (Catalog)
-- Fields from the Model protobuf message. Nested sub-messages (pricing,
-- metadata, specification) and repeated fields (categories, tags) are stored
-- as JSON columns.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS models (
    id                VARCHAR(36)   NOT NULL PRIMARY KEY,
    name              VARCHAR(255)  NOT NULL,
    provider          VARCHAR(255)  NOT NULL DEFAULT '',
    icon_bg           VARCHAR(255)  NOT NULL DEFAULT '',
    icon_text         VARCHAR(255)  NOT NULL DEFAULT '',
    icon_text_color   VARCHAR(255)  NOT NULL DEFAULT '',
    categories        JSON,
    is_new            BOOLEAN       NOT NULL DEFAULT FALSE,
    pricing           JSON,
    context_length    VARCHAR(255)  NOT NULL DEFAULT '',
    description       TEXT,
    metadata          JSON,
    specification     JSON,
    tags              JSON,

    created_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    INDEX idx_models_name (name),
    INDEX idx_models_provider (provider)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- API Keys
-- The key_hash stores the hashed secret for verification; key_prefix stores a
-- short displayable prefix (e.g. "aibrix_WSo2..."). The full plaintext key is
-- returned only once at creation time and is never persisted.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS api_keys (
    id                VARCHAR(36)   NOT NULL PRIMARY KEY,
    name              VARCHAR(255)  NOT NULL,
    key_hash          VARCHAR(255)  NOT NULL,
    key_prefix        VARCHAR(255)  NOT NULL DEFAULT '',
    created_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,

    INDEX idx_api_keys_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Secrets
-- The encrypted_value column holds the server-side encrypted secret value.
-- The plaintext value is never stored directly.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS secrets (
    id                VARCHAR(36)   NOT NULL PRIMARY KEY,
    name              VARCHAR(255)  NOT NULL,
    encrypted_value   TEXT          NOT NULL,
    created_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,

    INDEX idx_secrets_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Quotas
-- Tracks resource usage limits. usage_percentage is a derived convenience
-- column kept in sync by the application layer.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS quotas (
    id                VARCHAR(36)   NOT NULL PRIMARY KEY,
    name              VARCHAR(255)  NOT NULL,
    quota_id          VARCHAR(255)  NOT NULL,
    current_usage     INT           NOT NULL DEFAULT 0,
    usage_percentage  DOUBLE        NOT NULL DEFAULT 0.0,
    quota             INT           NOT NULL DEFAULT 0,

    created_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    UNIQUE INDEX idx_quotas_quota_id (quota_id),
    INDEX idx_quotas_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Users
-- Stores console users. external_id links to an external identity provider.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id                VARCHAR(36)   NOT NULL PRIMARY KEY,
    external_id       VARCHAR(255)  NOT NULL DEFAULT '',
    email             VARCHAR(255)  NOT NULL,
    name              VARCHAR(255)  NOT NULL DEFAULT '',
    role              VARCHAR(255)  NOT NULL DEFAULT 'viewer',
    created_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    UNIQUE INDEX idx_users_email (email),
    INDEX idx_users_external_id (external_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
