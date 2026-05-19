-- 001_initial.sql
--
-- Canonical MySQL schema reference for the AIBrix Console store.
--
-- IMPORTANT: this file is documentation. The runtime schema is created /
-- evolved by GORM AutoMigrate from `apps/console/api/store/models/*.go`
-- (see `gorm.go:RunMigrations`); this SQL is NOT executed. Keep it in sync
-- with the Go models when shipping schema changes so DBAs and reviewers
-- have a single readable artifact for the canonical schema.

-- ---------------------------------------------------------------------------
-- Deployments
-- StormService deployment records exposed via DeploymentService.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS deployments (
    id                 VARCHAR(36)   NOT NULL PRIMARY KEY,
    name               VARCHAR(255)  NOT NULL,
    deployment_id      VARCHAR(255)  NOT NULL,
    base_model         VARCHAR(255)  NOT NULL DEFAULT '',
    base_model_id      VARCHAR(255)  NOT NULL DEFAULT '',
    replicas           VARCHAR(255)  NOT NULL DEFAULT '1',
    gpus_per_replica   INT           NOT NULL DEFAULT 0,
    gpu_type           VARCHAR(255)  NOT NULL DEFAULT '',
    region             VARCHAR(255)  NOT NULL DEFAULT '',
    created_by         VARCHAR(255)  NOT NULL DEFAULT '',
    status             VARCHAR(255)  NOT NULL DEFAULT 'Deploying',

    -- StormService extension fields
    model_source       VARCHAR(255)  NOT NULL DEFAULT '',
    model_artifact_url VARCHAR(1000) NOT NULL DEFAULT '',
    engine             VARCHAR(255)  NOT NULL DEFAULT '',
    startup_command    TEXT,
    env_vars           JSON,
    extra_args         JSON,
    namespace          VARCHAR(255)  NOT NULL DEFAULT 'default',
    k8s_resource_name  VARCHAR(255)  NOT NULL DEFAULT '',

    created_at         TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    INDEX idx_deployments_name (name),
    INDEX idx_deployments_status (status),
    INDEX idx_deployments_created_by (created_by)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Jobs (Batch Inference)
--
-- Planner-owned state-machine snapshot of each batch job. One row per job.
-- The Console handler joins this row with MDS-fetched batch state at read
-- time to produce the wire-level pb.Job.
--
-- Status column holds one of the 13 plannerapi.JobStatus values (see
-- docs/source/designs/batch-job-state-machine.md):
--   pre-submit: queued, resource_preparing, submitting
--   post-submit (mirrors openai.Batch.status):
--     validating, in_progress, finalizing, cancelling
--   terminal: completed, failed, expired, cancelled,
--             resource_failed, submit_failed
--
-- Pre-submit timestamps (queued_at / resource_preparing_at / submitting_at)
-- are written by the Planner state machine. MDS-side timestamps
-- (in_progress_at / finalizing_at / completed_at / ...) are populated by
-- lazy sync in GetJob / ListJobs from openai.Batch.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS jobs (
    id                     VARCHAR(64)   NOT NULL PRIMARY KEY,
    endpoint               VARCHAR(255)  NOT NULL DEFAULT '',
    model                  VARCHAR(255)  NOT NULL DEFAULT '',
    input_dataset          VARCHAR(255)  NOT NULL DEFAULT '',
    completion_window      VARCHAR(64)   NOT NULL DEFAULT '',
    status                 VARCHAR(64)   NOT NULL DEFAULT '',
    output_dataset         VARCHAR(255)  NOT NULL DEFAULT '',
    error_dataset          VARCHAR(255)  NOT NULL DEFAULT '',

    -- MDS-side phase timestamps (lazy-synced from openai.Batch.*_at)
    batch_created_at       DATETIME      NULL,
    in_progress_at         DATETIME      NULL,
    expires_at             DATETIME      NULL,
    finalizing_at          DATETIME      NULL,
    completed_at           DATETIME      NULL,
    failed_at              DATETIME      NULL,
    expired_at             DATETIME      NULL,
    cancelling_at          DATETIME      NULL,
    cancelled_at           DATETIME      NULL,

    -- MDS-side aggregates (lazy-synced from openai.Batch)
    request_counts         JSON,
    usage                  JSON,
    metadata               JSON,

    -- Console-owned fields (extracted from metadata at write time)
    name                   VARCHAR(255)  NOT NULL DEFAULT '',
    created_by             VARCHAR(255)  NOT NULL DEFAULT '',
    model_template_name    VARCHAR(255)  NOT NULL DEFAULT '',
    model_template_version VARCHAR(64)   NOT NULL DEFAULT '',

    -- Planner state machine: foreign keys + pre-submit timestamps + error
    batch_id               VARCHAR(64)   NOT NULL DEFAULT '',
    provision_id           VARCHAR(64)   NOT NULL DEFAULT '',
    queued_at              DATETIME      NULL,
    resource_preparing_at  DATETIME      NULL,
    submitting_at          DATETIME      NULL,
    resource_failed_at     DATETIME      NULL,
    submit_failed_at       DATETIME      NULL,
    cancel_requested_at    DATETIME      NULL,
    error_message          TEXT,

    created_at             TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at             TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    INDEX idx_jobs_batch_id (batch_id),
    INDEX idx_jobs_status (status),
    INDEX idx_jobs_model (model),
    INDEX idx_jobs_created_at (created_at)
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
    serving_name      VARCHAR(255)  NOT NULL DEFAULT '',

    created_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    INDEX idx_models_name (name),
    INDEX idx_models_provider (provider)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Model Deployment Templates
-- Versioned deployment recipes bound to a parent model.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS model_deployment_templates (
    id                VARCHAR(36)   NOT NULL PRIMARY KEY,
    name              VARCHAR(255)  NOT NULL,
    version           VARCHAR(64)   NOT NULL,
    status            VARCHAR(64)   NOT NULL DEFAULT 'active',
    model_id          VARCHAR(36)   NOT NULL,
    spec              JSON          NOT NULL,

    created_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    UNIQUE INDEX uk_model_tpl_name_ver (model_id, name, version),
    INDEX idx_model_tpl_status (status, model_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- API Keys
-- key_hash stores the hashed secret for verification; key_prefix stores a
-- short displayable prefix (e.g. "aibrix_WSo2..."). Plaintext is returned
-- once at creation and never persisted.
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
-- encrypted_value holds the server-side encrypted secret value.
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
-- Resource usage limits. usage_percentage is a derived convenience column.
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
-- Provision Results
-- RM-internal record of each provision request. idempotency_key equals the
-- Console JobID (1:1 with jobs.id).
--
-- status holds one of the 7 ProvisionStatus values:
--   pending, provisioning, running, releasing, released, failed, release_failed
-- (K8sProvisioner only emits running / released / failed; others are reserved
-- for resource-allocating provisioners like AWS / LambdaCloud.)
--
-- provider names the backend that owns this provision; the Reconciler uses
-- it to dispatch Release calls to the right Provisioner implementation.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS provision_results (
    idempotency_key   VARCHAR(255)  NOT NULL PRIMARY KEY,
    provision_id      VARCHAR(255)  NOT NULL,
    provider          VARCHAR(32)   NOT NULL DEFAULT 'kubernetes',
    region            VARCHAR(255)  NOT NULL,
    status            VARCHAR(64)   NOT NULL,
    payload           JSON,
    created_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted           BOOLEAN       NOT NULL DEFAULT FALSE,

    UNIQUE INDEX uk_provision_results_provision_id (provision_id),
    INDEX idx_provision_results_region (region),
    INDEX idx_provision_results_provider (provider),
    INDEX idx_provision_results_status_deleted (status, deleted)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
