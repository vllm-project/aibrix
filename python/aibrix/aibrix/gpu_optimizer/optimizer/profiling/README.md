# GPU Profiling

## About
GPU performance toolkits. First, use `benchmark.sh` to get throughput, latency, and other SLOs for different length of inputs and outputs. Then `gpu-profile.py` can be used to create GPU profile that matches specified SLOs.

## Help on `benchmark.sh`
First, deploy your model of choice on the GPU you wish to profile. We support [vLLM](https://github.com/vllm-project/vllm/tree/main) like inference engine.

Once your model is up and running, modify `benchmark.sh` to configure the following parameters for the profiling:
* input_start: The starting input length for profling
* input_limit: The ending input length for profling
* output_start: The starting output length for profling
* output_limit: The ending output length for profling
* rate_start: The starting request rate for profling
* rate_limit: The ending request rate for profling
  
Run `pip install -r requirements.txt` to install dependency.

Finally, run `benchmark.sh [your deployment name]`, the results will be in the result directory.

## Help on `gpu-profile.py`

Run `python gen-profile.py -h` to see the help message.