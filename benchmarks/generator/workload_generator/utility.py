import json
from typing import List, Dict, Any
from pathlib import Path


def read_workload_file(file_path: str) -> List[Dict[str, Any]]:
    """Read a workload JSONL file and return a list of workload objects."""
    workloads = []
    with open(file_path, 'r') as f:
        for line in f:
            if line.strip():
                workloads.append(json.loads(line))
    return workloads


def write_workload_file(workloads: List[Dict[str, Any]], output_path: str):
    """Write workload objects to a JSONL file."""
    with open(output_path, 'w') as f:
        for workload in workloads:
            f.write(json.dumps(workload) + '\n')


def concat_workloads(workload_files: List[str], output_file: str):
    """
    Concatenate multiple workload files by adjusting timestamps of subsequent files.
    The timestamps in each subsequent file will be added to the last timestamp of the previous file.
    """
    all_workloads = []
    last_timestamp = 0

    for file_path in workload_files:
        workloads = read_workload_file(file_path)
        
        # Adjust timestamps for all workloads in this file
        for workload in workloads:
            workload['timestamp'] += last_timestamp
            all_workloads.append(workload)
        
        # Update last_timestamp for next file
        if workloads:
            last_timestamp = workloads[-1]['timestamp']

    write_workload_file(all_workloads, output_file)


def merge_workloads(workload_files: List[str], output_file: str):
    """
    Merge multiple workload files while preserving original timestamps.
    Requests with the same timestamp will be merged into a single workload object.
    The output will be sorted by timestamp.
    """
    # Dictionary to store workloads by timestamp
    timestamp_workloads: Dict[int, Dict[str, Any]] = {}

    for file_path in workload_files:
        workloads = read_workload_file(file_path)
        
        for workload in workloads:
            timestamp = workload['timestamp']
            
            if timestamp in timestamp_workloads:
                # Merge requests if timestamp exists
                existing_workload = timestamp_workloads[timestamp]
                existing_workload['requests'].extend(workload['requests'])
            else:
                # Create new workload entry if timestamp doesn't exist
                timestamp_workloads[timestamp] = workload

    # Convert dictionary to sorted list
    merged_workloads = [
        timestamp_workloads[timestamp]
        for timestamp in sorted(timestamp_workloads.keys())
    ]

    write_workload_file(merged_workloads, output_file)


def main():
    """Example usage of the workload utilities."""
    import argparse

    parser = argparse.ArgumentParser(description='Workload file manipulation utilities')
    parser.add_argument('--mode', choices=['concat', 'merge'], required=True,
                      help='Operation mode: concat or merge')
    parser.add_argument('--input-files', nargs='+', required=True,
                      help='Input workload files')
    parser.add_argument('--output-file', required=True,
                      help='Output workload file')

    args = parser.parse_args()

    if args.mode == 'concat':
        concat_workloads(args.input_files, args.output_file)
    else:  # merge
        merge_workloads(args.input_files, args.output_file)


if __name__ == '__main__':
    main() 