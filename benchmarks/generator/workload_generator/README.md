# Using Workload Generator

### Prerequisite

Our workload generator expects prompt collection files that follow one of the two data schema:
* .jsonl file with plain prompts collection **(the "completion" field is optional)**
```
{"prompt": XXX, "completion": AAA}
{"prompt": YYY, "completion": AAA}
{"prompt": ZZZ, "completion": AAA}
...
```
* .jsonl file with sesssioned prompts collection **(the "completions" field is optional)**
```
{"session_id": 0, "prompts": ["prompt 1", "prompt 2"], "completions": ["completion 1", "completion 2"]}
{"session_id": 1, "prompts": ["prompt 3", "prompt 4"], "completions": ["completion 3", "completion 4"]}
...
```
Please refer to [this](../dataset_generator/README.md) to create synthetic prompts or convert existing dataset to one of these formats before generating workloads. 


Ths workload generator would produce a workload file that looks like the following. Each logical timestamp is associated with list of prompts that need to be dispatched at the same time. 

```json
{
    "timestamp": 19, 
    "requests": 
    [
        {
            "prompt": "I need to understand data science...", 
            "prompt_length": 101, 
            "output_length": null,
            "session_id": 0
        },
        {
            "prompt": "...",
            "prompt_length": "...", 
            "output_length": "...",
            "session_id": "..."
        }
    ]
}
```

And it will also generate figures to illustrate this workload.

![workload-plot](workload-plot-example.png)


## Generate workload file

The workload generator currently supports the following workload types: static workload that supports static workload (QPS, input/output lengths), synthetic dynamic workload, grafana exported statistics, and actual LLM serving trace (Azure LLM trace). The output workload will be stored as a `workload.jsonl` under the output directory under `--output-dir`. Refer to the previous step to generate a `$PROMPT_FILE`.

> **Note** All generator invocation should be done under the benchmark home (i.e., `/aibrix/benchmarks/`)
### Generate a workload file based with constant target QPS (synthetic patterns)

```shell
export TARGET_QPS=1
export PROMPT_FILE="output/dataset/synthetic_shared.jsonl"

python -m generator.workload_generator.workload_generator \
    --prompt-file $PROMPT_FILE \
    --interval-ms 1000 \
    --duration-ms 300000 \
    --target-qps $TARGET_QPS \
    --trace-type constant \
    --model "Qwen/Qwen2.5-Coder-7B-Instruct" \
    --output-dir "output" \
    --output-format jsonl 
```

### Generate a workload file based on workload patterns (synthetic patterns)

The can generate workload file based on synthetic traffic (qps), input lengths (prompt lengths) and output lengths (completion lengths) patterns. Currently we support 4 patterns (`'quick_rising`, `'slow_rising'`, `'slight_fluctuation'`, `'severe_fluctuation'`), described [here](https://github.com/vllm-project/aibrix/blob/main/benchmarks/autoscaling/bench_workload_generator.py).:
```shell
python -m generator.workload_generator.workload_generator \
    --prompt-file $PROMPT_FILE \
    --interval-ms 1000 \
    --duration-ms 300000 \
    --trace-type synthetic \
    --traffic-pattern "slight_fluctuation" \
    --prompt-len-pattern "slight_fluctuation" \
    --completion-len-pattern "slight_fluctuation" \
    --model "Qwen/Qwen2.5-Coder-7B-Instruct" \
    --output-dir "./output" --output-format jsonl 
```

Alternatively, you could specify fluctuation patterns in .json file and pass to the generator like the following. Example configuration files are under `config` directory.
```shell
export TRAFFIC_PATTERN_FILE="generator/workload_generator/config/examples/traffic-config.json"
export PROMPT_LEN_FILE="generator/workload_generator/config/examples/prompt-len-config.json"
export COMPLETION_LEN_FILE="generator/workload_generator/config/examples/completion-len-config.json"
python -m generator.workload_generator.workload_generator \
    --prompt-file $PROMPT_FILE \
    --interval-ms 1000 \
    --duration-ms 1400000 \
    --trace-type synthetic \
    --traffic-pattern-config $TRAFFIC_PATTERN_FILE \
    --prompt-len-pattern-config $PROMPT_LEN_FILE \
    --completion-len-pattern-config $COMPLETION_LEN_FILE \
    --model "Qwen/Qwen2.5-Coder-7B-Instruct" \
    --output-dir "./output" \
    --output-format jsonl 
```


Here `--interval-ms` specifies the granularity of concurrent dispatched requests (in milliseconds). `--duration-ms` specifies the total length of the trace in milliseconds.

The file would be stored under `output` folder based on the name of different patterns. And the plot illustrates the workload pattern will be under the `plot` directory. 

### Generate a workload file based on Grafana exported .csv statistics files.

```shell
export TRAFFIC_FILE=${PATH_TO_TRAFFIC_FILE}
export PROMPT_LEN_FILE=${PATH_TO_PROMPT_LEN_FILE}
export COMPLETION_LEN_FILE=${PATH_TO_COMPLETION_LEN_FILE}

python -m generator.workload_generator.workload_generator \
    --prompt-file $PROMPT_FILE \
    --interval-ms 1000 \
    --duration-ms 1800000 \
    --trace-type stat \
    --traffic-file "$TRAFFIC_FILE" \
    --prompt-len-file "$PROMPT_LEN_FILE" \
    --completion-len-file "$COMPLETION_LEN_FILE"  \
    --model "Qwen/Qwen2.5-Coder-7B-Instruct" \
    --output-dir "./output" \
    --output-format jsonl \
    --qps-scale 1.0 \
    --output-scale 1.0 \
    --input-scale 1.0 \
    --stat-trace-type "maas" 
```

The scaling factor here (e.g., `qps-scale`) scale down rate from the original trace to the desired rate, i.e., if the peak rate in the original file is 80 and the desired peak rate is 8, the scale is set to 10.0. 

#### `maas` trace type 
- With `maas` trace type, the generator assumes the `$TRAFFIC_FILE` to be in the following format
```
"Time","Total","Success","4xx Error"
2024-10-1 00:00:00,100,99,1
```

- `"$PROMPT_LEN_FILE"` to be in the following format
```
"Time","P50","P70","P90","P99"
```

- `"$COMPLETION_LEN_FILE"` to be in the following format
```
"Time","P50","P70","P95","P99"
```

#### `cloudide` trace type 
- With `cloudide` trace type, the generator assumes the `$TRAFFIC_FILE` to be in the following format -- `"Rate"` column could have arbitrary names. 
```
"Time","Rate"
```

- `"$PROMPT_LEN_FILE"` to be in the following format
```
"Time","recv_bytes","sent_bytes"
```

- `"$COMPLETION_LEN_FILE"` to be in the following format
```
"Time","recv_bytes","sent_bytes"
```

#### Indicate the length of prompt/completion
In this case, you can also indicate the request's prompt length by the `--prompt-len-file` config, or the output length by the `--completion-len-file`,
based on the parameters, the generator will select the proper length in the prompt_file to simulate the length of the real flow's load.

The format of the file should follow the table head format and have the **exact same row length** as the traffic file
```
P50,P70,P99
2000,4000,10000
...
2000,4000,10000(same row size with traffic file)
```

And the plot illustrates the workload pattern will be under the `plot` directory. 


### Generate a workload file based on Azure LLM Trace

To produce a workload based on [Azure LLM Trace](https://github.com/Azure/AzurePublicDataset/tree/master/data), use the following commands:

```bash
wget https://raw.githubusercontent.com/Azure/AzurePublicDataset/refs/heads/master/data/AzureLLMInferenceTrace_conv.csv -O /tmp/AzureLLMInferenceTrace_conv.csv

export AZURE_TRACE_NAME=/tmp/AzureLLMInferenceTrace_conv.csv
python -m generator.workload_generator.workload_generator \
    --trace-file $AZURE_TRACE_NAME \
    --prompt-file $PROMPT_FILE \
    --interval-ms 1000 \
    --duration-ms 600000 \
    --trace-type azure \
    --group-interval-seconds 1 \
    --model "Qwen/Qwen2.5-Coder-7B-Instruct" \
    --output-dir "output"
```

Note that the trace file contains both input and output lengths. And therefore dataset in `$SHAREGPT_FILE_PATH` needs to be tokenized to be able to sampled based on their input/output token lengths. Therefore it is required to specify tokenizer to generate based on this trace. Use `--group-interval-seconds` to specify grouping interval from the original trace. The file would be stored under `output` folder and the plot illustrates the workload pattern will be under the `plot` directory.


Use [client](../client/README.md) to test generated trace locally. 