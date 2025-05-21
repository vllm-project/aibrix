import os
import sys
import subprocess
import yaml
import argparse
import logging 
from pathlib import Path
from client import (client, analyze)
from generator.dataset_generator import (synthetic_prefix_sharing_dataset, 
                                         multiturn_prefix_sharing_dataset, 
                                         utility)
from generator.workload_generator import workload_generator
from argparse import Namespace
from string import Template


class BenchmarkRunner:
    def __init__(self, config_base="config/base.yaml", overrides=None):
        self.overrides = overrides or []
        self.load_config(config_base)
        logging.warning(f"Loaded Config: {self.config}")
        for dir_key in ["dataset_dir", "workload_dir", "client_output", "trace_output"]:
            path_str = self.config[dir_key]
            self.ensure_directories(path_str)
            
    def apply_overrides(self, config_dict):
        for override in self.overrides:
            if '=' not in override:
                logging.warning(f"Invalid override format: {override}. Use key=value.")
                continue
            key, value = override.split("=", 1)
            try:
                parsed_value = yaml.safe_load(value)
            except Exception:
                parsed_value = value
            # Handle nested keys like "workload_configs.target_qps"
            parts = key.split(".")
            d = config_dict
            for p in parts[:-1]:
                if p not in d:
                    d[p] = {}
                d = d[p]
            d[parts[-1]] = parsed_value
            logging.info(f"Overridden {key} = {parsed_value}")
        return config_dict

    def load_config(self, config_path):
        if not Path(config_path).is_file():
            logging.error(f"{config_path} not found.")
            sys.exit(1)

        logging.info(f"Loading configuration from {config_path}")
        with open(config_path, 'r') as f:
            content = os.path.expandvars(f.read())
            raw_config = yaml.safe_load(content)
            raw_config = self.apply_overrides(raw_config)
        
        resolved_config = {}
        for key, value in raw_config.items():
            if isinstance(value, str):
                template = Template(value)
                value = template.safe_substitute(raw_config)
            resolved_config[key] = value
        self.config = resolved_config

    def ensure_directories(self, path_str):
        path = Path(path_str)
        path.mkdir(parents=True, exist_ok=True)

    def generate_dataset(self):
        dataset_type = self.config["prompt_type"]
        logging.info(f"Generating synthetic dataset {dataset_type}...")

        if dataset_type not in self.config["dataset_configs"]:
            logging.error(f"Unknown prompt type: {dataset_type}")
            sys.exit(1)

        subconfig = self.config["dataset_configs"][dataset_type]
        dataset_file = self.config["dataset_file"]  # Use the pre-defined dataset_file

        if dataset_type == "synthetic_shared":
            args_dict = {
                "output": dataset_file,
                "randomize_order": True,
                "tokenizer": self.config["tokenizer"],
                "app_name": self.config["prompt_type"],
                "prompt_length": subconfig["prompt_length"],
                "prompt_length_std": subconfig["prompt_std"],
                "shared_proportion": subconfig["shared_prop"],
                "shared_proportion_std": subconfig["shared_prop_std"],
                "num_samples_per_prefix": subconfig["num_samples"],
                "num_prefix": subconfig["num_prefix"],
                "num_configs": subconfig.get("num_dataset_configs", 1),
                "to_workload": False,
                "rps": 0,
            }
            args = Namespace(**args_dict)
            synthetic_prefix_sharing_dataset.main(args)
            
        elif dataset_type == "synthetic_multiturn":
            args_dict = {
                "output": dataset_file,
                "tokenizer": self.config["tokenizer"],
                "shared_prefix_len": subconfig["shared_prefix_length"],
                "prompt_length_mean": subconfig["prompt_length"],
                "prompt_length_std": subconfig["prompt_std"],
                "num_turns_mean": subconfig["num_turns"],
                "num_turns_std": subconfig["num_turns_std"],
                "num_sessions_mean": subconfig["num_sessions"],
                "num_sessions_std": subconfig["num_sessions_std"],
            }
            args = Namespace(**args_dict)
            multiturn_prefix_sharing_dataset.main(args)

        elif dataset_type == "client_trace":
            args_dict = {
                "command": "convert",
                "path": subconfig["trace"],
                "type": "trace",
                "output": dataset_file,
            }
            args = Namespace(**args_dict)
            utility.main(args)
            
        elif dataset_type == "sharegpt":
            if not Path(subconfig["target_dataset"]).is_file():
                print("[INFO] Downloading ShareGPT dataset...")
                subprocess.run([
                    "wget", "https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/resolve/main/ShareGPT_V3_unfiltered_cleaned_split.json",
                    "-O", subconfig["target_dataset"]
                ], check=True)
            args_dict = {
                "command": "convert",
                "path": subconfig["target_dataset"],
                "type": "sharegpt",
                "output": dataset_file,
            }
            args = Namespace(**args_dict)
            utility.main(args)

    def generate_workload(self):
        workload_type = self.config["workload_type"]
        print("[INFO] Generating workload...")
        
        if workload_type not in self.config["workload_configs"]:
            print(f"[ERROR] Unknown workload type: {workload_type}")
            sys.exit(1)

        subconfig = self.config["workload_configs"][workload_type]
        workload_type_dir = self.config["workload_dir"]
        self.ensure_directories(workload_type_dir)
        
        dataset_file = self.config["dataset_file"]  # Use the pre-defined dataset_file
        args_dict = {
            "prompt_file": dataset_file,
            "interval_ms": self.config["interval_ms"],
            "duration_ms": self.config["duration_ms"],
            "trace_type": workload_type,
            "model": self.config["model_name"],
            "output_dir": workload_type_dir,
            "output_format": "jsonl",
        }

        if workload_type == "constant":
            args_dict.update({
                "target_qps": subconfig["target_qps"],
                "target_prompt_len": subconfig["target_prompt_len"],
                "target_completion_len": subconfig["target_completion_len"],
                "max_concurrent_sessions": subconfig.get("max_concurrent_sessions", 1),
            })
            
        elif workload_type == "synthetic":
            args_dict.update({
                "traffic_pattern_config": subconfig["traffic_file"],
                "prompt_len_pattern_config": subconfig["prompt_len_file"],
                "completion_len_pattern_config": subconfig["completion_len_file"],
                "max_concurrent_sessions": subconfig.get("max_concurrent_sessions", 1),
            })
            
        elif workload_type == "stat":
            args_dict.update({
                "stat_trace_type": subconfig["stat_trace_type"],
                "traffic_file": subconfig["traffic_file"],
                "prompt_len_file": subconfig["prompt_len_file"],
                "completion_len_file": subconfig["completion_len_file"],
                "qps_scale": subconfig["qps_scale"],
                "output_scale": subconfig["output_scale"],
                "input_scale": subconfig["input_scale"],
            })
            
        elif workload_type == "azure":
            if not Path(subconfig["azure_trace"]).is_file():
                logging.info("Downloading Azure dataset...")
                subprocess.run([
                    "wget", "https://raw.githubusercontent.com/Azure/AzurePublicDataset/refs/heads/master/data/AzureLLMInferenceTrace_conv.csv",
                    "-O", subconfig["azure_trace"]
                ], check=True)
            args_dict.update({
                "traffic_file": subconfig["azure_trace"],
                "group_interval_seconds": 1,
            })

        args = Namespace(**args_dict)
        logging.info(f"Running workload generator with args: {args}")
        workload_generator.main(args)

    def run_client(self):
        logging.info("Running client to dispatch workload...")
        workload_file = self.config["workload_file"]  # Use the pre-defined workload_file
        
        args_dict = {
            "workload_path": workload_file,
            "endpoint": self.config["endpoint"],
            "model": self.config["target_model"],
            "api_key": self.config["api_key"],
            "output_file_path": f"{self.config['client_output']}/output.jsonl",
            "streaming": self.config.get("streaming_enabled", False),
            "routing_strategy": self.config.get("routing_strategy", "random"),
            "client_pool_size": self.config.get("client_pool_size", 128),
            "output_token_limit": self.config.get("output_token_limit", 128),
            "time_scale": self.config.get("time_scale", 1.0),
            "timeout_second": self.config.get("timeout_second", 60.0),
            "max_retries": self.config.get("max_retries", 0),
        }
        args = Namespace(**args_dict)
        logging.info(f"Running client with args: {args}")
        client.main(args)

    def run_analysis(self):
        logging.info("Analyzing trace output...")
        args_dict = {
            "trace": f"{self.config['client_output']}/output.jsonl",
            "output": self.config["trace_output"],
            "goodput_target": self.config["goodput_target"],
        }
        args = Namespace(**args_dict)
        logging.info(f"Running analysis with args: {args}")
        analyze.main(args)

    def run(self, command):
        logging.info("========== Starting Benchmark ==========")
        actions = {
            "dataset": self.generate_dataset,
            "workload": self.generate_workload,
            "client": self.run_client,
            "analysis": self.run_analysis,
            "all": lambda: [self.generate_dataset(), self.generate_workload(), self.run_client(), self.run_analysis()],
            "": lambda: [self.generate_dataset(), self.generate_workload(), self.run_client(), self.run_analysis()]
        }
        if command not in actions:
            logging.error(f"Unknown command: {command}")
            logging.error("Usage: script.py [dataset|workload|client|analysis|all]")
            sys.exit(1)
        result = actions[command]
        if callable(result):
            result()
        logging.info("========== Benchmark Completed ==========")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Run benchmark pipeline")
    parser.add_argument("--stage", required=True, help="One of the stages [all, dataset, workload, client, analysis]")
    parser.add_argument("--config", required=True, help="Path to base config YAML")
    parser.add_argument("--override", action="append", default=[], help="Override config values, e.g., --override time_scale=2.0 or target_qps=5")

    args = parser.parse_args()

    runner = BenchmarkRunner(config_base=args.config, overrides=args.override)
    runner.run(args.stage)
