import os
import subprocess
import argparse
import threading
import signal
import sys


def get_all_pods(namespace):
    pod_list_output = subprocess.check_output(['kubectl', 'get', 'pods', '-n', namespace, '-o', 'jsonpath={.items[*].metadata.name}'])
    pod_list = pod_list_output.decode('utf-8').split()
    return pod_list

def write_logs(keywords, fname, process):
    with open(fname, 'w') as log_file:
        while True:
            line = process.stdout.readline()
            if not line:
                break
            if len(keywords) == 0: # If there is no keyword, write all logs
                log_file.write(line)
                log_file.flush()
            else:
                for keyword in keywords:
                    if keyword in line:
                        # If there is keyword, write only the lines containing the keyword
                        log_file.write(line)
                        log_file.flush()
                        break

def save_proxy_logs_streaming(pod_log_dir, pod_name, namespace):
    if not os.path.exists(pod_log_dir):
        os.makedirs(pod_log_dir)
    
    fname = f"{pod_log_dir}/{pod_name}.streaming.pod.log.txt"
    print(f"** Saving {pod_name} logs in {namespace} namespace to {fname}")
    process = subprocess.Popen(
        ['kubectl', 'logs', '-f', pod_name, '-n', namespace],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        universal_newlines=True
    )
    if namespace == "default":
        keywords = ["Avg prompt throughput:", "logger.py", "engine.py"]
    else:
        keywords = []
    log_thread = threading.Thread(target=write_logs, args=(keywords, fname, process))
    log_thread.start()
    return process, log_thread

def signal_handler(sig, frame):
    print('\nStopping all log streaming processes...')
    for process, thread in running_processes:
        process.terminate()
    sys.exit(0)


if __name__ == "__main__":
    target_deployment = sys.argv[1]
    namespace = sys.argv[2]
    pod_log_dir = sys.argv[3]

    running_processes = []
    signal.signal(signal.SIGINT, signal_handler)
    all_pods = get_all_pods(namespace)
    if len(all_pods) == 0:
        print("Error, No pods found in the default namespace")
        assert False
    for pod_name in all_pods:
        if target_deployment in pod_name:
            process, thread = save_proxy_logs_streaming(pod_log_dir, pod_name, namespace)
            running_processes.append((process, thread))
    
    print(f"Started streaming logs for {len(all_pods)} pods")
    print("Press Ctrl+C to stop streaming")
    
    # Keep the script running
    try:
        while True:
            signal.pause()
    except (KeyboardInterrupt, SystemExit):
        signal_handler(None, None)