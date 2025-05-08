import os
import sys
import subprocess
import yaml
import argparse
from pathlib import Path
from client import (client, analyze)
from generator.dataset_generator import (synthetic_prefix_sharing_dataset, 
                                         multiturn_prefix_sharing_dataset, 
                                         utility)
from generator.workload_generator import workload_generator
from argparse import Namespace


class BenchmarkRunner:
    def __init__(self, config_base="config/base.yaml", overrides=None):
        self.overrides = overrides or []
        self.load_config(config_base)
        for dir_key in ["DATASET_DIR", "WORKLOAD_DIR", "CLIENT_OUTPUT", "TRACE_OUTPUT"]:
            path_str = self.config[dir_key]
            self.ensure_directories(path_str)
            
    def apply_overrides(self, config_dict):
        for override in self.overrides:
            if '=' not in override:
                print(f"[WARNING] Invalid override format: {override}. Use key=value.")
                continue
            key, value = override.split("=", 1)
            try:
                parsed_value = yaml.safe_load(value)
            except Exception:
                parsed_value = value
            # Handle nested keys like "WORKLOAD_CONFIG.TARGET_QPS"
            parts = key.split(".")
            d = config_dict
            for p in parts[:-1]:
                if p not in d:
                    d[p] = {}
                d = d[p]
            d[parts[-1]] = parsed_value
            print(f"[INFO] Overridden {key} = {parsed_value}")
        return config_dict

    def load_config(self, config_path):
        if not Path(config_path).is_file():
            print(f"[ERROR] {config_path} not found.")
            sys.exit(1)

        print(f"[INFO] Loading configuration from {config_path}")
        with open(config_path, 'r') as f:
            content = os.path.expandvars(f.read())
            self.config = yaml.safe_load(content)
            self.config = self.apply_overrides(self.config)

    def load_subconfig(self, subconfig_path):
        with open(subconfig_path, 'r') as f:
            content = os.path.expandvars(f.read())
            subconfig = yaml.safe_load(content)
            return self.apply_overrides(subconfig)

    def ensure_directories(self, path_str):
        path = Path(path_str)
        path.mkdir(parents=True, exist_ok=True)

    def generate_dataset(self):
        dataset_type = self.config["PROMPT_TYPE"]
        print(f"[INFO] Generating synthetic dataset {dataset_type}...")
        print(self.config)

        config_map = {item["name"]: item["config"] for item in self.config.get("DATASET_CONFIG", [])}

        print(f"loaded config_map {config_map}")
        if dataset_type not in config_map:
            print(f"[ERROR] Unknown prompt type: {dataset_type}")
            sys.exit(1)

        subconfig = self.load_subconfig(config_map[dataset_type])
        dataset_file = f'{self.config["DATASET_DIR"].rstrip("/")}/{self.config["PROMPT_TYPE"]}.jsonl'


        if dataset_type == "synthetic_shared":
            args_dict = {
                "output": dataset_file,
                "randomize_order": True,
                "tokenizer": self.config["TOKENIZER"],
                "app_name": self.config["PROMPT_TYPE"],
                "prompt_length": subconfig["PROMPT_LENGTH"],
                "prompt_length_std": subconfig["PROMPT_STD"],
                "shared_proportion": subconfig["SHARED_PROP"],
                "shared_proportion_std": subconfig["SHARED_PROP_STD"],
                "num_samples_per_prefix": subconfig["NUM_SAMPLES"],
                "num_prefix": subconfig["NUM_PREFIX"],
                "num_configs": subconfig.get("NUM_DATASET_CONFIGS", 1),
                "to_workload": False,
                "rps": 0,
            }
            args = Namespace(**args_dict)
            synthetic_prefix_sharing_dataset.main(args)
            
        elif dataset_type == "synthetic_multiturn":
            args_dict = {
                "output": dataset_file,
                "tokenizer": self.config["TOKENIZER"],
                "shared_prefix_len": subconfig["SHARED_PREFIX_LENGTH"],
                "prompt_length_mean": subconfig["PROMPT_LENGTH"],
                "prompt_length_std": subconfig["PROMPT_STD"],
                "num_turns_mean": subconfig["NUM_TURNS"],
                "num_turns_std": subconfig["NUM_TURNS_STD"],
                "num_sessions_mean": subconfig["NUM_SESSIONS"],
                "num_sessions_std": subconfig["NUM_SESSIONS_STD"],
            }
            args = Namespace(**args_dict)
            multiturn_prefix_sharing_dataset.main(args)

        elif dataset_type == "client_trace":
            args_dict = {
                "command": "convert",
                "path": subconfig["TRACE"],
                "type": "trace",
                "output": dataset_file,
            }
            args = Namespace(**args_dict)
            utility.main(args)
            
        elif dataset_type == "sharegpt":
            if not Path(subconfig["TARGET_DATASET"]).is_file():
                print("[INFO] Downloading ShareGPT dataset...")
                subprocess.run([
                    "wget", "https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/resolve/main/ShareGPT_V3_unfiltered_cleaned_split.json",
                    "-O", subconfig["TARGET_DATASET"]
                ], check=True)
            args_dict = {
                "command": "convert",
                "path": subconfig["TARGET_DATASET"],
                "type": "sharegpt",
                "output": dataset_file,
            }
            args = Namespace(**args_dict)
            utility.main(args)
            

    def generate_workload(self):
        workload_type = self.config["WORKLOAD_TYPE"]
        print("[INFO] Generating workload...")
        config_map = {item["name"]: item["config"] for item in self.config.get("WORKLOAD_CONFIG", [])}
        if workload_type not in config_map:
            print(f"[ERROR] Unknown workload type: {workload_type}")
            sys.exit(1)
        config_path = config_map[workload_type]
        if not Path(config_path).is_file():
            print(f"[ERROR] Unsupported workload type: {workload_type}")
            sys.exit(1)

        subconfig = self.load_subconfig(config_path)
        workload_dir = self.config["WORKLOAD_DIR"]
        workload_type_dir = f"{workload_dir}/{workload_type}"
        self.ensure_directories(workload_type_dir)
        args_dict = {
            "prompt_file": None,  # required, no default
            "trace_type": None,   # required, no default
            "model": "Qwen/Qwen2.5-Coder-7B-Instruct",
            "interval_ms": 1000,
            "duration_ms": 60000,
            "group_interval_seconds": 1,
            "stat_trace_type": "maas",
            "output_dir": "output",
            "output_format": "json",

            # Synthetic and constant workload
            "target_qps": 1,
            "target_prompt_len": None,
            "target_completion_len": None,
            "traffic_pattern": None,
            "prompt_len_pattern": None,
            "completion_len_pattern": None,
            "traffic_pattern_config": None,
            "prompt_len_pattern_config": None,
            "completion_len_pattern_config": None,

            # Trace and stats-driven workload
            "traffic_file": None,
            "prompt_len_file": None,
            "completion_len_file": None,
            "qps_scale": 1.0,
            "input_scale": 1.0,
            "output_scale": 1.0,
            "max_concurrent_sessions": 1,
        }
        dataset_file = f'{self.config["DATASET_DIR"].rstrip("/")}/{self.config["PROMPT_TYPE"]}.jsonl'
        if workload_type == "constant":
            args_dict["prompt_file"] = dataset_file
            args_dict["interval_ms"] = self.config["INTERVAL_MS"]
            args_dict["duration_ms"] = self.config["DURATION_MS"]
            args_dict["target_qps"] = subconfig["TARGET_QPS"]
            args_dict["trace_type"] = workload_type
            args_dict["model"] = self.config["MODEL_NAME"]
            args_dict["output_dir"] = workload_type_dir
            args_dict["output_format"] = "jsonl"
            args_dict["max_concurrent_sessions"] = subconfig.get("MAX_CONCURRENT_SESSIONS", 1)
            
            args = Namespace(**args_dict)
            workload_generator.main(args)
        elif workload_type == "synthetic":
            args_dict["prompt_file"] = dataset_file
            args_dict["interval_ms"] = self.config["INTERVAL_MS"]
            args_dict["duration_ms"] = self.config["DURATION_MS"]
            args_dict["trace_type"] = workload_type
            args_dict["model"] = self.config["MODEL_NAME"]
            args_dict["output_dir"] = workload_type_dir
            args_dict["output_format"] = "jsonl"
            args_dict["traffic_pattern_config"] = subconfig.get("TRAFFIC_FILE")
            args_dict["prompt_len_pattern_config"] = subconfig.get("PROMPT_LEN_FILE")
            args_dict["completion_len_pattern_config"] = subconfig.get("COMPLETION_LEN_FILE")
            args_dict["max_concurrent_sessions"] = subconfig.get("MAX_CONCURRENT_SESSIONS", 1)
            
            print(f"[INFO] Generating synthetic workload with args: {args_dict}")
            args = Namespace(**args_dict)
            workload_generator.main(args)
        elif workload_type == "stat":
            args_dict["prompt_file"] = dataset_file
            args_dict["interval_ms"] = self.config["INTERVAL_MS"]
            args_dict["duration_ms"] = self.config["DURATION_MS"]
            args_dict["trace_type"] = workload_type
            args_dict["model"] = self.config["MODEL_NAME"]
            args_dict["output_dir"] = workload_type_dir
            args_dict["output_format"] = "jsonl"
            args_dict["stat_trace_type"] = subconfig.get("STAT_TRACE_TYPE")
            args_dict["traffic_file"] = subconfig.get("TRAFFIC_FILE")
            args_dict["prompt_len_file"] = subconfig.get("PROMPT_LEN_FILE")
            args_dict["completion_len_file"] = subconfig.get("COMPLETION_LEN_FILE")
            args_dict["qps_scale"] = subconfig.get("QPS_SCALE")
            args_dict["output_scale"] = subconfig.get("OUTPUT_SCALE")
            args_dict["input_scale"] = subconfig.get("INPUT_SCALE")
            
            args = Namespace(**args_dict)
            workload_generator.main(args)
            
        elif workload_type == "azure":
            args_dict["prompt_file"] = dataset_file
            args_dict["interval_ms"] = self.config["INTERVAL_MS"]
            args_dict["duration_ms"] = self.config["DURATION_MS"]
            args_dict["trace_type"] = workload_type
            args_dict["model"] = self.config["MODEL_NAME"]
            args_dict["output_dir"] = workload_type_dir
            args_dict["output_format"] = "jsonl"
            args_dict["traffic_file"] = subconfig.get("AZURE_TRACE")
            args_dict["group_interval_seconds"] = 1
            print(args_dict)
            args = Namespace(**args_dict)
            workload_generator.main(args)

    def run_client(self):
        print("[INFO] Running client to dispatch workload...")
        workload_dir = self.config["WORKLOAD_DIR"]
        workload_type = self.config["WORKLOAD_TYPE"]
        workload_file =f"{workload_dir}/{workload_type}/workload.jsonl"
        
        args_dict = {
            "workload_path": workload_file,
            "endpoint": self.config["ENDPOINT"],
            "model": self.config["TARGET_MODEL"],
            "api_key": self.config["API_KEY"],
            "output_file_path": f"{self.config['CLIENT_OUTPUT']}/output.jsonl",
            "streaming": self.config.get("STREAMING_ENABLED", False),
            "routing_strategy": self.config.get("ROUTING_STRATEGY", "random"),
            "client_pool_size": self.config.get("CLIENT_POOL_SIZE", 128),
            "output_token_limit": self.config.get("OUTPUT_TOKEN_LIMIT", 128),
            "time_scale": self.config.get("TIME_SCALE", 1.0),
        }
        args = Namespace(**args_dict)
        print(f"[INFO] Running client with args: {args}")
        client.main(args)

    def run_analysis(self):
        print("[INFO] Analyzing trace output...")
        args_dict = {
            "trace": f"{self.config['CLIENT_OUTPUT']}/output.jsonl",
            "output": self.config["TRACE_OUTPUT"],
            "goodput_target": self.config["GOODPUT_TARGET"],
        }
        args = Namespace(**args_dict)
        print(f"[INFO] Running analysis with args: {args}")
        analyze.main(args)

    def run(self, command):
        print("========== Starting Benchmark ==========")
        actions = {
            "dataset": self.generate_dataset,
            "workload": self.generate_workload,
            "client": self.run_client,
            "analysis": self.run_analysis,
            "all": lambda: [self.generate_dataset(), self.generate_workload(), self.run_client(), self.run_analysis()],
            "": lambda: [self.generate_dataset(), self.generate_workload(), self.run_client(), self.run_analysis()]
        }
        if command not in actions:
            print(f"[ERROR] Unknown command: {command}")
            print("Usage: script.py [dataset|workload|client|analysis|all]")
            sys.exit(1)
        result = actions[command]
        if callable(result):
            result()
        print("========== Benchmark Completed ==========")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Run benchmark pipeline")
    parser.add_argument("command", nargs="?", default="all", help="One of [dataset, workload, client, analysis, all]")
    parser.add_argument("--config", default="config/base.yaml", help="Path to base config YAML")
    parser.add_argument("--override", action="append", default=[], help="Override config values, e.g., --override TIME_SCALE=2.0 or TARGET_QPS=5")

    args = parser.parse_args()

    runner = BenchmarkRunner(config_base=args.config, overrides=args.override)
    runner.run(args.command)
