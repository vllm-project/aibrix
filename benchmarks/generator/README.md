# Generate workload file

## Generate a workload file based on .csv trace file
```
export TRACE_FILE=${PATH_TO_TRACE_FILE}
export SHARE_GPT_PATH=${PATH_TO_SHARE_GPT_FILE}
python workload_generator.py --prompt-file $SHARE_GPT_PATH --num-prompts 100 --num-requests 100000 --trace-file "$TRACE_FILE" --output "from_csv.json"
```

This generator assumes trace file to be in the following format
```
"Time","Total","Success","4xx Error"
2024-10-1 00:00:00,100,99,1
```

This generator generate workload file (in .json format) under ```traces``` folder. The file would look like the following:
```
[
    [["Prompt1", prompt_len_1, output_len_1, null],["Prompt2", prompt_len_2, output_len_2, null], ...],
    [["Prompt3", prompt_len_3, output_len_3, null],["Prompt4", prompt_len_4, output_len_4, null], ...],
    ...
]

```

## Generate a workload file based on workload patterns
If no trace file path is specified, the generator will generate workload file based on 4 synthetic pattern described [here](https://github.com/aibrix/aibrix/blob/main/benchmarks/autoscaling/bench_workload_generator.py):

```
python workload_generator.py --prompt-file $SHARE_GPT_PATH --num-prompts 100 --num-requests 10000
```

The file would be stored under ```traces``` folder based on the name of different patterns.