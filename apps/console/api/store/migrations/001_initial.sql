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
    row_id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'Auto-increment primary key',
    id                 VARCHAR(36)   NOT NULL DEFAULT '' COMMENT 'UUID for application lookups',
    name               VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'User-visible deployment name',
    deployment_id      VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Short 8-char ID for K8s resource naming',
    base_model         VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Model display name (e.g., llama-2-7b)',
    base_model_id      VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Derived model ID for lookups (lowercase, hyphenated)',
    min_replicas       INT           NOT NULL DEFAULT 1 COMMENT 'Minimum number of replicas',
    max_replicas       INT           NOT NULL DEFAULT 1 COMMENT 'Maximum number of replicas',
    gpus_per_replica   INT           NOT NULL DEFAULT 0 COMMENT 'GPUs per replica',
    gpu_type           VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'GPU type',
    region             VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Deployment region',
    created_by         VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Creator user ID',
    status             VARCHAR(255)  NOT NULL DEFAULT 'Deploying' COMMENT 'Deployment status',
    template_id        VARCHAR(36)   NOT NULL DEFAULT '' COMMENT 'Deployment template UUID',
    template_version   VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Deployment template version',
    provider_kind      VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Deployment provider kind',
    model_source       VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Model source location',
    model_artifact_url VARCHAR(1000) NOT NULL DEFAULT '' COMMENT 'Model artifact URL',
    engine             VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Inference engine type',
    startup_command    TEXT                   COMMENT 'Custom startup command',
    env_vars           JSON                   COMMENT 'Environment variables',
    extra_args         JSON                   COMMENT 'Extra command arguments',
    namespace          VARCHAR(255)  NOT NULL DEFAULT 'default' COMMENT 'K8s namespace',
    k8s_resource_name  VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'K8s resource name',
    deleted            BOOLEAN       NOT NULL DEFAULT FALSE COMMENT 'Soft delete flag',
    created_at         TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Creation timestamp',
    updated_at         TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT 'Last update timestamp',

    PRIMARY KEY (row_id),
    UNIQUE INDEX uniq_deployments_id (id),
    INDEX idx_deployments_name (name),
    INDEX idx_deployments_status (status),
    INDEX idx_deployments_created_by (created_by),
    INDEX idx_deployments_base_model (base_model),
    INDEX idx_deployments_deleted (deleted)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Deployment records';

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
    row_id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'Auto-increment primary key',
    id                    VARCHAR(64)   NOT NULL DEFAULT '' COMMENT 'Job ID (UUID for application lookups)',
    endpoint              VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'API endpoint for job',
    model                 VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Model name',
    input_dataset         VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Input dataset path',
    completion_window     VARCHAR(64)   NOT NULL DEFAULT '' COMMENT 'Completion window',
    status                VARCHAR(64)   NOT NULL DEFAULT '' COMMENT 'Job status',
    output_dataset        VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Output dataset path',
    error_dataset         VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Error dataset path',
    batch_created_at      TIMESTAMP     NULL COMMENT 'MDS batch creation timestamp',
    in_progress_at        TIMESTAMP     NULL COMMENT 'MDS in-progress timestamp',
    expires_at            TIMESTAMP     NULL COMMENT 'MDS expiration timestamp',
    finalizing_at         TIMESTAMP     NULL COMMENT 'MDS finalizing timestamp',
    completed_at          TIMESTAMP     NULL COMMENT 'MDS completion timestamp',
    failed_at             TIMESTAMP     NULL COMMENT 'MDS failure timestamp',
    expired_at            TIMESTAMP     NULL COMMENT 'MDS expired timestamp',
    cancelling_at         TIMESTAMP     NULL COMMENT 'MDS cancelling timestamp',
    cancelled_at          TIMESTAMP     NULL COMMENT 'MDS cancelled timestamp',
    request_counts        JSON               COMMENT 'Request count statistics',
    usage_stats           JSON               COMMENT 'Usage statistics',
    metadata              JSON               COMMENT 'Job metadata',
    name                  VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'User-visible job name',
    created_by            VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Creator user ID',
    model_template_name   VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Model template name',
    model_template_version VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'Model template version',
    batch_id              VARCHAR(64)   NOT NULL DEFAULT '' COMMENT 'MDS batch ID',
    provision_id          VARCHAR(36)   NOT NULL DEFAULT '' COMMENT 'Provision result ID',
    queued_at             TIMESTAMP     NULL COMMENT 'Queued state timestamp',
    resource_preparing_at TIMESTAMP     NULL COMMENT 'Resource preparing timestamp',
    submitting_at         TIMESTAMP     NULL COMMENT 'Submitting timestamp',
    resource_failed_at    TIMESTAMP     NULL COMMENT 'Resource failed timestamp',
    submit_failed_at      TIMESTAMP     NULL COMMENT 'Submit failed timestamp',
    cancel_requested_at   TIMESTAMP     NULL COMMENT 'Cancel requested timestamp',
    error_msg             TEXT               COMMENT 'Error message',
    deleted               BOOLEAN       NOT NULL DEFAULT FALSE COMMENT 'Soft delete flag',
    created_at            TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Creation timestamp',
    updated_at            TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT 'Last update timestamp',

    PRIMARY KEY (row_id),
    UNIQUE INDEX uniq_jobs_id (id),
    INDEX idx_jobs_batch_id (batch_id),
    INDEX idx_jobs_status (status),
    INDEX idx_jobs_model (model),
    INDEX idx_jobs_created_at (created_at),
    INDEX idx_jobs_deleted (deleted)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Batch inference job records';

-- ---------------------------------------------------------------------------
-- Models (Catalog)
-- Fields from the Model protobuf message. Nested sub-messages (pricing,
-- metadata, specification) and repeated fields (categories, tags) are stored
-- as JSON columns.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS models (
    row_id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'Auto-increment primary key',
    id               VARCHAR(36)   NOT NULL DEFAULT '' COMMENT 'Model UUID for application lookups',
    name             VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Model display name',
    provider         VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Model provider name',
    icon_bg          VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Icon background color',
    icon_text        VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Icon text',
    icon_text_color  VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Icon text color',
    categories       JSON                   COMMENT 'Model categories',
    is_new           BOOLEAN       NOT NULL DEFAULT FALSE COMMENT 'Is new model flag',
    pricing          JSON                   COMMENT 'Pricing information',
    context_length   VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Context length (e.g., 128k)',
    description      TEXT                   COMMENT 'Model description',
    metadata         JSON                   COMMENT 'Model metadata',
    specification    JSON                   COMMENT 'Model specification',
    tags             JSON                   COMMENT 'Model tags',
    serving_name     VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Serving endpoint name',
    deleted          BOOLEAN       NOT NULL DEFAULT FALSE COMMENT 'Soft delete flag',
    created_at       TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Creation timestamp',
    updated_at       TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT 'Last update timestamp',

    PRIMARY KEY (row_id),
    UNIQUE INDEX uniq_models_id (id),
    INDEX idx_models_name (name),
    INDEX idx_models_provider (provider),
    INDEX idx_models_deleted (deleted)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Model catalog records';

-- ---------------------------------------------------------------------------
-- Model Deployment Templates
-- Versioned deployment recipes bound to a parent model.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS model_deployment_templates (
    row_id     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'Auto-increment primary key',
    id         VARCHAR(36)   NOT NULL DEFAULT '' COMMENT 'Template UUID for application lookups',
    name       VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Template name',
    version    VARCHAR(64)   NOT NULL DEFAULT '' COMMENT 'Template version',
    status     VARCHAR(64)   NOT NULL DEFAULT 'active' COMMENT 'Template status',
    model_id   VARCHAR(36)   NOT NULL DEFAULT '' COMMENT 'Parent model ID',
    spec       JSON                   COMMENT 'Template specification',
    deleted    BOOLEAN       NOT NULL DEFAULT FALSE COMMENT 'Soft delete flag',
    created_at TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Creation timestamp',
    updated_at TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT 'Last update timestamp',

    PRIMARY KEY (row_id),
    UNIQUE INDEX uniq_model_tpl_id (id),
    UNIQUE INDEX uniq_model_tpl_name_ver (model_id, name, version),
    INDEX idx_model_tpl_status (status, model_id),
    INDEX idx_model_tpl_deleted (deleted)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Model deployment template records';

-- ---------------------------------------------------------------------------
-- API Keys
-- key_hash stores the hashed secret for verification; key_prefix stores a
-- short displayable prefix (e.g. aibrix_WSo2...). Plaintext is returned
-- once at creation and never persisted.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS api_keys (
    row_id     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'Auto-increment primary key',
    id         VARCHAR(36)   NOT NULL DEFAULT '' COMMENT 'API key UUID for application lookups',
    name       VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'API key name',
    key_hash   VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Hashed key secret',
    key_prefix VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Key prefix for display',
    deleted    BOOLEAN       NOT NULL DEFAULT FALSE COMMENT 'Soft delete flag',
    created_at TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Creation timestamp',

    PRIMARY KEY (row_id),
    UNIQUE INDEX uniq_api_keys_id (id),
    INDEX idx_api_keys_name (name),
    INDEX idx_api_keys_deleted (deleted)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='API key records';

-- ---------------------------------------------------------------------------
-- Secrets
-- encrypted_value holds the server-side encrypted secret value.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS secrets (
    row_id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'Auto-increment primary key',
    id              VARCHAR(36)   NOT NULL DEFAULT '' COMMENT 'Secret UUID for application lookups',
    name            VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Secret name',
    encrypted_value TEXT                   COMMENT 'Encrypted secret value',
    deleted         BOOLEAN       NOT NULL DEFAULT FALSE COMMENT 'Soft delete flag',
    created_at      TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Creation timestamp',

    PRIMARY KEY (row_id),
    UNIQUE INDEX uniq_secrets_id (id),
    INDEX idx_secrets_name (name),
    INDEX idx_secrets_deleted (deleted)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Secret records';

-- ---------------------------------------------------------------------------
-- Quotas
-- Resource usage limits. usage_percentage is computed at query time.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS quotas (
    row_id        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'Auto-increment primary key',
    id            VARCHAR(36)   NOT NULL DEFAULT '' COMMENT 'Quota UUID for application lookups',
    name          VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Quota name',
    quota_id      VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Quota identifier',
    current_usage INT           NOT NULL DEFAULT 0 COMMENT 'Current usage count',
    quota         INT           NOT NULL DEFAULT 0 COMMENT 'Quota limit',
    deleted       BOOLEAN       NOT NULL DEFAULT FALSE COMMENT 'Soft delete flag',
    created_at    TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Creation timestamp',
    updated_at    TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT 'Last update timestamp',

    PRIMARY KEY (row_id),
    UNIQUE INDEX uniq_quotas_id (id),
    UNIQUE INDEX uniq_quotas_quota_id (quota_id),
    INDEX idx_quotas_name (name),
    INDEX idx_quotas_deleted (deleted)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Resource quota records';

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
    row_id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'Auto-increment primary key',
    idempotency_key VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Idempotency key for deduplication',
    provision_id    VARCHAR(36)   NOT NULL DEFAULT '' COMMENT 'Provision ID',
    provider        VARCHAR(32)   NOT NULL DEFAULT 'kubernetes' COMMENT 'Provisioner provider',
    region          VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Provision region',
    status          VARCHAR(64)   NOT NULL DEFAULT '' COMMENT 'Provision status',
    payload         JSON                   COMMENT 'Provision payload',
    created_at      TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Creation timestamp',
    updated_at      TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Last update timestamp',
    deleted         BOOLEAN       NOT NULL DEFAULT FALSE COMMENT 'Soft delete flag',

    PRIMARY KEY (row_id),
    UNIQUE INDEX uniq_provision_results_idempotency_key (idempotency_key),
    UNIQUE INDEX uniq_provision_results_provision_id (provision_id),
    INDEX idx_provision_results_region (region),
    INDEX idx_provision_results_provider (provider),
    INDEX idx_provision_results_status_deleted (status, deleted)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Provision result records';
