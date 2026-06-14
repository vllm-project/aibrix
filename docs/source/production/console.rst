.. _aibrix_console:

==============
AIBrix Console
==============

AIBrix Console is an optional web control plane for AIBrix. It provides a UI
for model deployment workflows, OpenAI-compatible batch jobs, and playground
traffic against an AIBrix gateway. The same workflows remain available through
Kubernetes resources and APIs, so the Console can be introduced independently.

The Console is made of three parts:

* a React frontend in ``apps/console/web``
* a Go backend in ``cmd/console`` and ``apps/console/api``
* integrations with the Metadata Service (MDS), the AIBrix gateway, and a
  persistent Console store

Run locally
-----------

Build the backend from the repository root:

.. code-block:: bash

   make build-console

Start the backend in development auth mode:

.. code-block:: bash

   STORE_URI=sqlite:/tmp/aibrix-console.db \
   METADATA_SERVICE_URL=http://localhost:8090 \
   GATEWAY_ENDPOINT=http://localhost:8888 \
   AUTH_MODE=dev \
   DEV_MODE=true \
   ./bin/console --http-addr :8080 --grpc-addr :50060

Start the frontend development server:

.. code-block:: bash

   cd apps/console/web
   npm install
   npm run dev

The frontend development server listens on ``http://localhost:3000``. The
backend REST API listens on ``http://localhost:8080`` by default.

Serve a production frontend build
---------------------------------

Build the frontend assets and serve them from the Go backend:

.. code-block:: bash

   cd apps/console/web
   VITE_ENABLE_DEPLOYMENTS=false VITE_ENABLE_PLAYGROUND=false npm run build

   cd ../../..
   # Add auth settings from the Authentication section before production use.
   STATIC_FILES_DIR=apps/console/web/dist \
   STORE_URI=mysql://aibrix:password@mysql:3306/aibrix \
   AUTH_MODE=oidc \
   ./bin/console --http-addr :8080

Use a stable ``STORE_URI``, ``SESSION_SECRET``, and ``SECRETS_ENCRYPTION_KEY``
for any environment where user sessions, credentials, or job history must
survive process restarts.

Storage
-------

The Console backend stores users, sessions, templates, job metadata, and
secrets in the configured store. Set the store with ``STORE_URI``:

.. list-table::
   :header-rows: 1

   * - Store
     - URI example
     - Use case
   * - SQLite
     - ``sqlite:/var/lib/aibrix/console.db``
     - Single-instance deployments and local testing
   * - In-memory
     - ``memory://`` or ``sqlite::memory:``
     - Short-lived development only
   * - MySQL
     - ``mysql://user:pass@host:3306/aibrix``
     - Shared or production deployments

SQLite paths under ``/tmp`` are convenient for local testing but are not
appropriate for durable environments.

Authentication
--------------

Set ``AUTH_MODE`` to choose how users sign in.

.. list-table::
   :header-rows: 1

   * - Mode
     - Required settings
     - Notes
   * - ``dev``
     - none
     - Uses ``DEV_USER_NAME`` and ``DEV_USER_EMAIL``. Intended for local use.
   * - ``basic``
     - ``BASIC_USERNAME``, ``BASIC_PASSWORD``, ``SESSION_SECRET``,
       ``SECRETS_ENCRYPTION_KEY``
     - Simple password login for restricted environments.
   * - ``oidc``
     - ``OIDC_ISSUER_URL``, ``OIDC_CLIENT_ID``, ``OIDC_REDIRECT_URL``,
       ``SESSION_SECRET``, ``SECRETS_ENCRYPTION_KEY``
     - Use an external identity provider. ``OIDC_CLIENT_SECRET`` is required
       when the provider or signing algorithm requires it.

Example OIDC configuration:

.. code-block:: bash

   export AUTH_MODE=oidc
   export OIDC_ISSUER_URL=https://issuer.example.com
   export OIDC_CLIENT_ID=aibrix-console
   export OIDC_CLIENT_SECRET=replace-me
   export OIDC_REDIRECT_URL=https://console.example.com/api/v1/auth/callback
   export OIDC_ADMIN_GROUPS=aibrix-admins
   export SESSION_SECRET="$(openssl rand -hex 32)"
   export SECRETS_ENCRYPTION_KEY="$(openssl rand -hex 32)"

Register the redirect URL with the identity provider. For production, serve the
Console over HTTPS and keep ``SESSION_SECRET`` and ``SECRETS_ENCRYPTION_KEY``
stable across restarts.

OIDC authorization can be restricted with these settings:

* ``OIDC_GROUPS_CLAIM``: JWT claim used to read groups. Defaults to
  ``groups``.
* ``OIDC_ADMIN_GROUPS``: comma-separated groups that receive admin access.
* ``OIDC_ADMIN_EMAILS``: comma-separated email addresses that receive admin
  access.
* ``OIDC_SIGNING_ALG``: token signing algorithm. Defaults to ``RS256``.

Configuration reference
-----------------------

.. list-table::
   :header-rows: 1

   * - Setting
     - Default
     - Purpose
   * - ``HTTP_ADDR``
     - ``:8080``
     - Backend HTTP and REST API address. Also available as
       ``--http-addr``.
   * - ``GRPC_ADDR``
     - ``:50060``
     - Backend gRPC address. Also available as ``--grpc-addr``.
   * - ``STORE_URI``
     - ``sqlite:/tmp/aibrix-console.db``
     - Console database connection string. Also available as
       ``--store-uri``.
   * - ``METADATA_SERVICE_URL``
     - ``http://localhost:8090``
     - MDS endpoint used for files and OpenAI-compatible batch APIs.
   * - ``GATEWAY_ENDPOINT``
     - ``http://localhost:8888``
     - AIBrix gateway endpoint used by the playground and inference flows.
       Also available as ``--gateway-endpoint``.
   * - ``DEFAULT_BATCH_MODEL_DEPLOYMENT_TEMPLATE``
     - empty
     - Default model deployment template name for Console-created batch jobs.
   * - ``PROVISIONER``
     - ``kubernetes``
     - Batch resource provisioner. Supported values include ``kubernetes``,
       ``runpod``, and ``lambdaCloud``. See :ref:`batch_resource_manager`.
   * - ``PLANNING_POLICY``
     - ``simple``
     - Planning policy used by the batch resource manager.
   * - ``PLANNER_WORKER_COUNT``
     - ``10``
     - Number of planner workers for batch scheduling.
   * - ``STATIC_FILES_DIR``
     - empty
     - Directory that contains the built Console frontend assets.
   * - ``ALLOWED_ORIGINS``
     - empty
     - Comma-separated CORS origins for browser clients.
   * - ``DEV_MODE``
     - ``false``
     - Enables development-oriented backend behavior and logging. Also
       available as ``--dev-mode``.

Batch workflow
--------------

The Console batch workflow uses the same backend concepts as the Batch API and
Batch Resource Manager:

1. Create or select a model deployment template.
2. Upload or select an input JSONL file through MDS.
3. Create a batch job from the Console.
4. The Console stores the job metadata and submits the batch request to MDS.
5. The planner and resource manager provision resources when configured.
6. Monitor job status and download output or error files after completion.

For the underlying API and resource planning behavior, see :ref:`batch_api`,
:ref:`batch_model_deployment_templates`, and :ref:`batch_resource_manager`.

Feature flags
-------------

The web frontend uses build-time feature flags:

.. list-table::
   :header-rows: 1

   * - Flag
     - Purpose
   * - ``VITE_ENABLE_DEPLOYMENTS``
     - Enables deployment management UI.
   * - ``VITE_ENABLE_PLAYGROUND``
     - Enables the playground UI.

Both flags are enabled by default when running the frontend development server.
Production builds should set them explicitly so the generated assets match the
intended Console surface:

.. code-block:: bash

   VITE_ENABLE_DEPLOYMENTS=false VITE_ENABLE_PLAYGROUND=false npm run build
