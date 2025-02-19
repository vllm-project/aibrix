# Using Workload Generator

## Generate workload file

### Prerequisite

```shell
wget https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/resolve/main/ShareGPT_V3_unfiltered_cleaned_split.json -O /tmp/ShareGPT_V3_unfiltered_cleaned_split.json
export SHAREGPT_FILE_PATH=/tmp/ShareGPT_V3_unfiltered_cleaned_split.json
```

### Generate a workload file based with constant target QPS (synthetic patterns)

```shell
export TARGET_QPS=1

python workload_generator.py --prompt-file $SHAREGPT_FILE_PATH --interval-ms 1000 --duration-ms 300000 --target-qps $ta --trace-type constant --model "Qwen/Qwen2.5-Coder-7B-Instruct" --output-dir "output" --output-format jsonl 
```

### Generate a workload file based on workload patterns (synthetic patterns)

The can generate workload file based on synthetic traffic (qps), input lengths (prompt lengths) and output lengths (completion lengths) patterns. Currently we support 4 patterns (`'quick_rising`, `'slow_rising'`, `'slight_fluctuation'`, `'severe_fluctuation'`), described [here](https://github.com/vllm-project/aibrix/blob/main/benchmarks/autoscaling/bench_workload_generator.py).:
```shell
python workload_generator.py --prompt-file $SHAREGPT_FILE_PATH --interval-ms 1000 --duration-ms 300000 --trace-type synthetic --traffic-pattern "slight_fluctuation" --prompt-len-pattern "slight_fluctuation" --completion-len-pattern "slight_fluctuation" --model "Qwen/Qwen2.5-Coder-7B-Instruct" --output-dir "./output" --output-format jsonl 
```

Alternatively, you could specify fluctuation patterns in .json file and pass to the generator like the following. Example configuration files are under `config` directory.
```shell
python workload_generator.py --prompt-file $SHAREGPT_FILE_PATH --interval-ms 1000 --duration-ms 1400000 --trace-type synthetic --traffic-pattern-config config/traffic-config.json --prompt-len-pattern-config config/prompt-len-config.json --completion-len-pattern-config config/completion-len-config.json --model "Qwen/Qwen2.5-Coder-7B-Instruct" --output-dir "./output" --output-format jsonl 
```


Here `--interval-ms` specifies the granularity of concurrent dispatched requests (in milliseconds). `--duration-ms` specifies the total length of the trace in milliseconds.

The file would be stored under `output` folder based on the name of different patterns. And the plot illustrates the workload pattern will be under the `plot` directory. 

## Generate a workload file based on internal load summary .csv file

```shell
export TRAFFIC_FILE=${PATH_TO_TRAFFIC_FILE}
export PROMPT_LEN_FILE=${PATH_TO_PROMPT_LEN_FILE}
export COMPLETION_LEN_FILE=${PATH_TO_COMPLETION_LEN_FILE}

python workload_generator.py --prompt-file $SHAREGPT_FILE_PATH --interval-ms 1000 --duration-ms 1800000 --trace-type internal --traffic-file "$TRAFFIC_FILE" --prompt-len-file "$PROMPT_LEN_FILE" --completion-len-file "$COMPLETION_LEN_FILE"  --model "Qwen/Qwen2.5-Coder-7B-Instruct" --output-dir "./output" --output-format jsonl --qps-scale 1.0 --output-scale 1.0 --input-scale 1.0 --internal-trace-type "maas" 
```

The scaling factor here (e.g., `qps-scale`) scale down rate from the original trace to the desired rate, i.e., if the peak rate in the original file is 80 and the desired peak rate is 8, the scale is set to 10.0. 

### `maas` trace type 
- With `maas` trace type, the generator assumes the `$TRAFFIC_FILE` to be in the following format
```
"Time","Total","Success","4xx Error"
2024-10-1 00:00:00,100,99,1
```

- `"$PROMPT_LEN_FILE"` to be in the following format
```
"Time","P50","P70","P90","P99"
```

- `"$PROMPT_LEN_FILE"` to be in the following format
```
"Time","P50","P70","P95","P99"
```

### `cloudide` trace type 
- With `cloudide` trace type, the generator assumes the `$TRAFFIC_FILE` to be in the following format -- `"Rate"` column could have arbitrary names. 
```
"Time","Rate"
```

- `"$PROMPT_LEN_FILE"` to be in the following format
```
"Time","recv_bytes","sent_bytes"
```

- `"$PROMPT_LEN_FILE"` to be in the following format
```
"Time","recv_bytes","sent_bytes"
```

### Indicate the length of prompt/completion
In this case, you can also indicate the request's prompt length by the `--prompt-len-file` config, or the output length by the `--completion-len-file`,
based on the parameters, the generator will select the proper length in the prompt_file to simulate the length of the real flow's load.

The format of the file should follow the table head format and have the **exact same row length** as the traffic file
```
P50,P70,P99
2000,4000,10000
...
2000,4000,10000(same row size with traffic file)
```

This generator generate workload file (in .json format) under `output` folder. The file would look like the following:
```
[
    [["Prompt1", prompt_len_1, output_len_1, null],["Prompt2", prompt_len_2, output_len_2, null], ...],
    [["Prompt3", prompt_len_3, output_len_3, null],["Prompt4", prompt_len_4, output_len_4, null], ...],
    ...
]
```

And the plot illustrates the workload pattern will be under the `plot` directory. 


## Generate a workload file based on Azure LLM Trace

To produce a workload based on [Azure LLM Trace](https://github.com/Azure/AzurePublicDataset/tree/master/data), use the following commands:

```
wget https://raw.githubusercontent.com/Azure/AzurePublicDataset/refs/heads/master/data/AzureLLMInferenceTrace_conv.csv -O /tmp/AzureLLMInferenceTrace_conv.csv
export AZURE_TRACE_NAME=/tmp/AzureLLMInferenceTrace_conv.csv
python workload_generator.py --prompt-file $SHAREGPT_FILE_PATH --num-prompts 100 --interval-ms 1000 --duration-ms 600000 --trace-type azure --trace-file "$AZURE_TRACE_NAME" --group-interval-seconds 1 --model "Qwen/Qwen2.5-Coder-7B-Instruct" --output-dir "output"
```

Note that the trace file contains both input and output lengths. And therefore dataset in `$SHAREGPT_FILE_PATH` needs to be tokenized to be able to sampled based on their input/output token lengths. Therefore it is required to specify tokenizer to generate based on this trace. Use `--group-interval-seconds` to specify grouping interval from the original trace. The file would be stored under `output` folder and the plot illustrates the workload pattern will be under the `plot` directory.

## Run Workload Generator

Starting vllm server:

```shell
python3 -m vllm.entrypoints.openai.api_server --host 0.0.0.0 \
--port "8000" \
--model /root/models/deepseek-coder-6.7b-instruct \
--trust-remote-code \
--max-model-len "14304" \
--api-key sk-kFJ12nKsFVfVmGpj3QzX65s4RbN2xJqWzPYCjYu7wT3BlbLi \
--enable-chunked-prefill
```

Using a sample workload in a client:

```shell
python3 client.py \
--workload-path "output/quick_rising.jsonl" \
--endpoint "http://localhost:8000" \
--model /root/models/deepseek-coder-6.7b-instruct \
--api-key sk-kFJ12nKsFVfVmGpj3QzX65s4RbN2xJqWzPYCjYu7wT3BlbLi \
--output-file-path output.jsonl
```

The output will be stored as a `.jsonl` file in `output.jsonl`
