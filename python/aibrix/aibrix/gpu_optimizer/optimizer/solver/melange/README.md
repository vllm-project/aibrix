# Mélange: Cost Efficient Large Language Model Serving by Exploiting GPU Heterogeneity

## About
Here we provide the implementation of the Mélange solver and other related scripts used in our [paper](https://arxiv.org/pdf/2404.14527).

## Getting Started
```bash
# Tested on Python 3.9.18

# 1. Install the necessary dependencies
pip install -r requirements.txt

# See the melange/profiling/profiling-instructions.md for instructions on how to obtain the GPU information needed as the solver's input.

# 2. Execute the solver with your own input configuration
python -m melange.main -c melange/config/example.json

# 3. By default, the solver will save the output in a JSON file named as "melange_result.json" at the root directory
```


## Explanation of Inputs and Outputs
### Inputs
The solver requires a json file with the following inputs:
1. `workload_distribution`: A 2D matrix representing the distribution of input and output lengths that the LLM service expects. Each row refers to one input size, each column refer to one output size, and each cell correspond to the proportion of requests that are within the cell's input and output size range (i.e., a bucket). The request size boundaries between buckets can be tuned to reach a desired balance of granularity and solver complexity. An example for the range of input and output sizes could be as follows:
    - Input/Output size: 1-25, 25-100, 100-250, ...
    - The cell at (0, 0) represents the request rate for requests with input and output sizes of 1-25 tokens.
    - The cell at (0, 1) represents the request rate for requests with input size 1-25 tokens and output size 25-100 tokens.
    - And so on ...
2. `gpu_info`: A list of dictionaries, where each dictionary contains the following keys:
    - `name`: The name of the GPU.
    - `cost`: The hourly rental cost of the GPU.
    - `tputs`: A 2D matrix where each cell represents the GPU's profiled maximum throughput for requests of size equivalent to the corresponding cell in the `workload_distribution` matrix.
3. `total_request_rate`: A float value representing the total request rate of the workload.
4. `slice_factor`: An integer multiplier for the number of slices each bucket is split into.

Please kindly refer to [example.json](melange/config/example.json) for an example of the inputs and check out our paper for more details on our methodology. We have also provided the profiling scripts we used to obtain the GPU information in the [profiling](melange/profiling) directory. See the [profiling instructions](melange/profiling/profiling-instructions.md) for more details on how to use these scripts.

### Outputs
### Solver Output
The solver returns a dictionary containing the following:
1. The name of each GPU and the number of that GPU type to use.
2. The total cost for one hour.

An example of the solver output is as follows:
```json
{
    "A10G": 3,
    "A100-80GB": 1,
    "cost": 6.7
}
```
In this case, the solver recommends using 3 A10G GPUs and 1 A100-80GB GPUs, which results in a total cost of $6.7/hr.

### Output Formats
Melange currently supports the following output formats:
* **JSON**:
  * Default output format.
  * The solver output is saved as a JSON file at the root directory with the name `melange_result.json`.

## Run with Your Own Dataset or GPU Information
The toy example at [script_code](melange/main.py) and [example_config](melange/config/example.json) includes examples of the four inputs to Mélange, which should be replaced to fit your setting's need.

### Workload Distribution
   1. Determine the expected distribution of request sizes your LLM service expects. For example, you can use historical data of requests served by your service. In our evaluations, we used publicly available datasets (such as [Chatbot Arena](https://huggingface.co/datasets/lmsys/lmsys-chat-1m)) to determine a reasonable distribution of request sizes.
   2. Populate the `workload_distribution` based on the determined distribution. As mentioned, each row refers to a single input size, each column refers to a single output size, and each cell corresponds to the proportion of requests that fall into the given bucket. For example, a cell value of 0.1 indicates that 10% are in that bucket's size range.

### GPU Information
For each GPU instance of interest, provide the following information:
   1. The name of the instance.
   2. The hourly rental cost of the instance.
   3. Results from profiling the GPUs maximum throughput (in requests/s) for requests within each bucket's size range from the buckets in `workload_distribution`.

### Overall Rate and Slice Factor
 1. Determine the service's overall request rate across all request sizes, and provide it as the `total_request_rate`.
 2. Decide on the slice factor. We find that the solver's output is not very sensitive to the choice of slice factor. We empirically find that 4 is sufficient for most cases.

## For Arm-based Mac platforms
We have occasionally (but not always) seen errors using PuLP on Arm-based MACs (M1/M2/M3). If you experience this issue, it's likely because the default ILP solver used by the PuLP library is not compatible with your architecture and will require additional steps.
1. Install the COIN CBC ILP solver using homebrew: `brew install coin-or-tools/coinor/cbc`
2. In [melange/solver.py](melange/solver.py), uncomment the following code to use the CBC solver. Note that your `path` may differ based on where the library was installed.
```
solver= pulp.getSolver('COIN_CMD', path='/opt/homebrew/opt/cbc/bin/cbc', msg=0)
problem.solve(solver)
```

## Citation
If you use Mélange in your research, please cite our [paper](https://arxiv.org/abs/2404.14527):
```
@article{griggs2024m,
  title={M$\backslash$'elange: Cost Efficient Large Language Model Serving by Exploiting GPU Heterogeneity},
  author={Griggs, Tyler and Liu, Xiaoxuan and Yu, Jiaxiang and Kim, Doyoung and Chiang, Wei-Lin and Cheung, Alvin and Stoica, Ion},
  journal={arXiv preprint arXiv:2404.14527},
  year={2024}
}
```
