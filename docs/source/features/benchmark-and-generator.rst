.. _benchmark:

=================================
Benchmark and Workload Generator
=================================
`AIBrix Benchmark <https://github.com/vllm-project/aibrix/tree/main/benchmarks>`_ contains the following components:

- Dataset (Prompt) Generation
- Workload Generation
- Benchmark Client
- Benchmark Scenarios (Work-in-progress)

The diagram below shows the end-to-end steps of AIBrix benchmarks. Our components are highlighted as green rectangles in this diagram.
  
.. figure:: ../assets/images/benchmark/aibrix-benchmark-component-doc.png
  :alt: benchmark-component-doc
  :width: 100%
  :align: center


Run AIBrix Benchmark End-to-End
--------------------------------------

.. note::
    The benchmark script benchmark.sh `benchmark.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/benchmark.sh>`_ performs all steps up to the AIBrix workload format and trigger benchmark client without setting up benchmark environment for different scenarios. It assumes that AIBrix is already set up and expects a fully responsive endpoint.
 


First, make sure you have configured your API key as well as your endpoint like

.. code-block:: bash

    export api_key="<your_api_key>"
    kubectl -n envoy-gateway-system port-forward service/<gateway_service> 8888:80 &


To run all steps using the default setting, try

.. code-block:: bash

    ./benchmark.sh all


If the script completes successfully, you should see something similar to the following output:

.. code-block:: bash

    INFO:root:Benchmark completed in 35.26 seconds
    [INFO] Analyzing trace output...
    End-to-End Latency (s) Statistics: Average = 15.4749, Median = 13.4021, 99th Percentile = 34.8283
    Throughput Statistics: Average = 34.3082, Median = 37.1284, 99th Percentile = 49.6834
    Tokens per Second Statistics: Average = 53.2558, Median = 50.5690, 99th Percentile = 83.0661
    Prompt Tokens Statistics: Average = 288.1000, Median = 148.5000, 99th Percentile = 805.6400
    Output Tokens Statistics: Average = 580.5000, Median = 461.5000, 99th Percentile = 1462.6000
    Total Tokens Statistics: Average = 868.6000, Median = 656.0000, 99th Percentile = 2268.2400
    Time to First Token (TTFT) Statistics: Average = 4.0021, Median = 4.0541, 99th Percentile = 7.5454
    Time per Output Token (TPOT) Statistics: Average = 0.0195, Median = 0.0196, 99th Percentile = 0.0200
    Errors Statistics: Average = 0.0000, Median = 0.0000, 99th Percentile = 0.0000
    Goodput (reqs/s) 1.0000
    ========== Benchmrk Completed ==========
    
    

All default shared environment variables can be found in  `config <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/>`_ directory.



Run Dataset Generator
-----------------------

.. figure:: ../../../benchmarks/image/aibrix-benchmark-dataset.png
  :alt: benchmark-component-dataset-generator
  :width: 70%
  :align: center

The AIBrix workload generator accepts either time-series traces (e.g., Open-source LLM trace, Grafana exported time-series metrics, see this for more details) or synthetic prompt file which could be hand-tuned by users (i.e., synthetic dataset format). 
A synthetic dataset needs to be in one of the two formats:

- Plain format (no sessions)

.. code-block:: bash
    
    {"prompt": "XXXX"}
    {"prompt": "YYYY"}
    

- Session format

.. code-block:: bash

    {"session_id": 0, "prompts": ["XXX", "YYY"]}
    {"session_id": 1, "prompts": ["AAA", "BBB", "CCC"]}

The dataset generator either generates a prompt dataset or converts an existing dataset which belongs to one of the two formats above. 


To run dataset generation, do


.. code-block:: bash

    ./benchmark dataset



Currently, we support four types of dataset. 

**1. Controlled Synthetic Sharing**
- This type allows users to generate a cache sharing *sessioned-format* dataset with *controlled prompt token length* and *controlled prefix sharing length*, as well as controlled number of prefixes (i.e., sessions). To tune the prompt token length and shared length, set environment variables in `config/dataset/synthetic_shared.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/dataset/synthetic_shared.sh>`_.

**2. Multiturn Synthetic**
- Multiturn synthetic data generation produces *sessioned-format* dataset. Each session ID maps to a *controlled number of prompts* per session and *controlled prompt lengths*. These variables could be tuned via `config/dataset/synthetic_multiturn.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/dataset/synthetic_multiturn.sh>`_. 

**3. ShareGPT**
- This generation type converts ShareGPT dataset to *sessioned-format* dataset that has session_id, prompts and completions. Configure via `config/dataset/sharegpt.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/dataset/sharegpt.sh>`_.

**4. Client trace**
- This generation type converts client output into a *plain-format* dataset. Configure via `config/dataset/client_trace.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/dataset/client_trace.sh>`_.

The first two types generate synthetic prompts, while the latter two convert external data sources or benchmark data.


To set the type of dataset to be generated, set the environment variable `PROMPT_TYPE` in `config/base.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/base.sh>`_ to one of the following values: ```synthetic_multiturn```, ```synthetic_shared```, ```sharegpt```, ```client_trace```.

For details of dataset generator, check out `dataset-generator <https://github.com/vllm-project/aibrix/blob/main/benchmarks/generator/dataset-generator>`_ directory. All tunable parameters are set under `config/dataset <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/dataset>`_.


Run Workload Generator
-----------------------

.. figure:: ../../../benchmarks/image/aibrix-benchmark-workload.png
  :alt: benchmark-component-workload-generator
  :width: 70%
  :align: center

The workload generator specifies the timing and requests to be dispatched in a workload. A workload generator accepts either a trace/metrics files (where either time and requests are specified, or QPS/input/output volume are specified) or a synthetic dataset format that contains prompts and possibly session. There are three types of workload the generator currently supports. 

**1. The "constant" and "synthetic" workload type**

- The workload generator can produce two types of *synthetic load pattern*, with multiple workload configurations that can be manually tuned (e.g., traffic/QPS distribution, input request token length distribution, output token length distribution, maximum concurrent sessions, etc.):
    - Constant load (**constant**): The mean load (QPS/input length/output length) stays constant with controallable fluctuation. Configure this type via `config/workload/constant.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/workload/constant.sh>`_.
    - Synthetic fluctuation load (**synthetic**): The loads (QPS/input length/output length) fluctuate based on configurable parameters. Configure this type via `config/workload/synthetic.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/workload/synthetic.sh>`_.

**2. The "stat" workload type**

- For *metrics file (e.g., .csv file exported from Grafana dashboard)*, the workload generator will generate the QPS/input length/output length distribution that follows the collected time-series metrics specified in the file. The actual prompt used in the workload, will be based on one of the synthetic dataset generated by the [dataset generator](#run-dataset-generator). Configure this workload type via `config/workload/stat.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/workload/stat.sh>`_.


**3. The "azure" workload type**

- For a trace (e.g., Azure LLM trace), both the requests and timestamp associated with the requests are provided, and the workload generator will generate a workload that simply replay requests based on the timestamp. Configure this type via `config/workload/azure.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/workload/azure.sh>`_.


Workload generator could be run by:

.. code-block:: bash

    ./benchmark workload



To choose different workload type, set the environment variable `WORKLOAD_TYPE` in `config/base.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/base.sh>`_ to one of the following values: ```constant```, ```synthetic```, ```stat```, ```azure```.


For details of workload generator, check out `workload-generator <https://github.com/vllm-project/aibrix/blob/main/benchmarks/generator/workload-generator>`_. All tunable parameters are set under `config/workload <https://github.com/vllm-project/aibrix/tree/main/benchmarks/config/workload>`_.


Run Benchmark Client
-----------------------

.. figure:: ../../../benchmarks/image/aibrix-benchmark-client.png
  :alt: benchmark-component-workload-generator
  :width: 30%
  :align: center

The benchmark client supports both batch and streaming mode. Streaming mode supports intra-request metrics like TTFT/TPOT. Configure endpoint and target model via `config/base.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/base.sh>`_.

The benchmark client can be run using:

.. code-block:: bash

    ./benchmark client



Run Analysis
-----------------------

Run analysis on the benchmark results using:

.. code-block:: bash

    ./benchmark analysis

Configure path and performance target via `config/base.sh <https://github.com/vllm-project/aibrix/blob/main/benchmarks/config/base.sh>`_.
