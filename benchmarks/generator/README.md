# Using Workload Generator

## Generate workload file

### Prerequisite

```shell
wget https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/resolve/main/ShareGPT_V3_unfiltered_cleaned_split.json -O /tmp/ShareGPT_V3_unfiltered_cleaned_split.json
export SHAREGPT_FILE_PATH=/tmp/ShareGPT_V3_unfiltered_cleaned_split.json
```

### Generate a workload file based on workload patterns (synthetic patterns)

If no trace file path is specified, the generator will generate workload file based on 4 synthetic pattern described [here](https://github.com/aibrix/aibrix/blob/main/benchmarks/autoscaling/bench_workload_generator.py):
```shell
python workload_generator.py --prompt-file $SHAREGPT_FILE_PATH --num-prompts 100 --interval-ms 1000 --duration-ms 600000 --trace-type synthetic --model "Qwen/Qwen2.5-Coder-7B-Instruct" --output-dir "output" 
```
Here `--interval-ms` specifies the granularity of concurrent dispatched requests (in milliseconds). `--duration-ms` specifies the total length of the trace in milliseconds.

The file would be stored under `output` folder based on the name of different patterns. And the plot illustrates the workload pattern will be under the `plot` directory. 

## Generate a workload file based on internal load summary .csv file

```shell
export SUMMARY_FILE=${PATH_TO_SUMMARY_FILE}
python workload_generator.py --prompt-file $SHAREGPT_FILE_PATH --num-prompts 100 --interval-ms 1000 --duration-ms 600000 --trace-type internal --trace-file "$SUMMARY_FILE" --model "Qwen/Qwen2.5-Coder-7B-Instruct" --output-dir "output"
```

This generator assumes trace file to be in the following format
```
"Time","Total","Success","4xx Error"
2024-10-1 00:00:00,100,99,1
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
