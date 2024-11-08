# AI Runtime
A versatile sidecar enabling metric standardization, model downloading, and management.

## Quick Start
### Installation
AI Runtime can be installed by `pip`.

```sh
pip install aibrix
```

### Model download
The AI Runtime supports model downloading from the following storage backends:
* HuggingFace
* S3
* TOS

For more details on model downloading, please refer to our [Runtime docs](https://github.com/aibrix/aibrix/blob/main/docs/source/features/runtime.rst#model-downloading).

### Integrate with inference engines
The AI Runtime hides various implementation details on the inference engine side, providing a universal method to guide model management, as well as expose inference monitoring metrics.

At present, `vLLM` engine is supported, and in the future, `SGLang` and other inference engines will be supported.

For more details on integrate with `vLLM`, please refer to our [Runtime docs](https://github.com/aibrix/aibrix/blob/main/docs/source/features/runtime.rst#metric-standardization).

## Contributing
We welcome contributions from the community! Check out our [contributing guidelines](https://github.com/aibrix/aibrix/blob/main/CONTRIBUTING.md) to see how you can make a difference.

### Build from source

```bash
# This may take several minutes
pip install -e .
```

### Lint, Format and Type Check

Before contribute your code, please run the following commands to ensure that your code passes the tests and linting checks.

```bash
# install dependencies
poetry install --no-root --with dev

# linting, formatting and type checking
bash ./scripts/format.sh
```

## License

AI Runtime is licensed under the [APACHE License](https://github.com/aibrix/aibrix/LICENSE.md).