.. _ai_runtime:

==========
AI Runtime
==========

AI Runtime is a versatile sidecar enabling metric standardization, model downloading, and management.

The AI Runtime hides various implementation details on the inference engine side, providing a universal method to guide model download and management, as well as expose inference monitoring metrics.

.. attention:: 
    Before using the  AI Runtime feature, ensure that the Python package for the aibrix is installed correctly.

    ``python3 -m pip install aibrix``



Metric Standardization
----------------------
Different inference engines will expose different metrics, and AI Runtime will standardize them.

Define the information related to the inference engine side in the container environment variables. For example, if ``vLLM`` provides metrics services on ``http://localhost:8000/metrics``, launch the AI Runtime Server by the following command:

.. code-block:: bash

    INFERENCE_ENGINE=vllm INFERENCE_ENGINE_ENDPOINT="http://localhost:8000" aibrix_runtime --port 8080


And AI Runtime will provide unified inference metrics on ``http://localhost:8080/metrics``.

Model Downloading
------------------
The AI Runtime provides support for downloading models from multiple remote sources, currently supporting HuggingFace, S3, and TOS.


Download From HuggingFace
^^^^^^^^^^^^^^^^^^^^^^^^^^
First Define the necessary environment variables for the HuggingFace model.

.. code-block:: bash

    # General settings
    export DOWNLOADER_ALLOW_FILE_SUFFIX=json, safetensors
    export DOWNLOADER_NUM_THREADS=16
    # HuggingFace settings
    export HF_ENDPOINT=https://hf-mirror.com  # set it when env is in CN


Then use AI Runtime to download the model from HuggingFace:

.. code-block:: bash

    python -m aibrix.downloader \
        --model-uri deepseek-ai/deepseek-coder-6.7b-instruct \
        --local-dir /tmp/aibrix/models_hf/


Download From S3
^^^^^^^^^^^^^^^^^
First Define the necessary environment variables for the S3 model.

.. code-block:: bash

    # General settings
    export DOWNLOADER_ALLOW_FILE_SUFFIX=json, safetensors
    export DOWNLOADER_NUM_THREADS=16
    # AWS settings
    export AWS_ACCESS_KEY_ID=<INPUT YOUR AWS ACCESS KEY ID>
    export AWS_SECRET_ACCESS_KEY=<INPUT YOUR AWS SECRET ACCESS KEY>
    export AWS_ENDPOINT_URL=<INPUT YOUR AWS ENDPOINT URL>
    export AWS_REGION=<INPUT YOUR AWS REGION>


Then use AI Runtime to download the model from AWS S3:

.. code-block:: bash

    python -m aibrix.downloader \
        --model-uri s3://aibrix-model-artifacts/deepseek-coder-6.7b-instruct/ \
        --local-dir /tmp/aibrix/models_s3/
    

Download From TOS
^^^^^^^^^^^^^^^^^
First Define the necessary environment variables for the TOS model.

.. code-block:: bash

    # General settings
    export DOWNLOADER_ALLOW_FILE_SUFFIX=json, safetensors
    export DOWNLOADER_NUM_THREADS=16
    # AWS settings
    export TOS_ACCESS_KEY=<INPUT YOUR TOS ACCESS KEY>
    export TOS_SECRET_KEY=<INPUT YOUR TOS SECRET KEY>
    export TOS_ENDPOINT=<INPUT YOUR TOS ENDPOINT>
    export TOS_REGION=<INPUT YOUR TOS REGION>


Then use AI Runtime to download the model from TOS:

.. code-block:: bash

    python -m aibrix.downloader \
        --model-uri tos://aibrix-model-artifacts/deepseek-coder-6.7b-instruct/ \
        --local-dir /tmp/aibrix/models_tos/
    


