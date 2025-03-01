from kubernetes import client, config
import csv
from datetime import datetime
import argparse
import time
import os
import asyncio

def get_pod_status_counts(deployment_name, namespace="default"):
   config.load_kube_config(context="ccr3aths9g2gqedu8asdg@41073177-kcu0mslcp5mhjsva38rpg")
   v1 = client.CoreV1Api()
   pods = v1.list_namespaced_pod(namespace)
   filtered_pods = [pod for pod in pods.items if deployment_name in pod.metadata.name]
   status_counts = {"running": 0, "pending": 0, "init": 0}
   for pod in filtered_pods:
    #    print(f"Pod: {pod.metadata.name}")
       if pod.status.container_statuses:
           if pod.status.phase == "Running":
               status_counts["running"] += 1
           elif pod.status.phase == "Pending":
               status_counts["pending"] += 1
       elif pod.status.init_container_statuses:
           status_counts["init"] += 1
   return status_counts

def write_to_csv(deployment_name, status_counts, filename, idx):
    unixtime = time.time()
    datetimestampe = datetime.now().strftime("%Y-%m-%d_%H-%M-%S")
    asyncio_time = asyncio.get_event_loop().time()
    file_exists = os.path.isfile(filename)
    mode = 'a' if file_exists else 'w'
    with open(filename, mode, newline='') as f_:
        writer = csv.writer(f_)
        if not file_exists:
            writer.writerow(["Deployment", "Running", "Pending", "Init", "datetimestampe", "unixtime", "asyncio_time"])
        writer.writerow([deployment_name, status_counts["running"], status_counts["pending"], status_counts["init"], datetimestampe, unixtime, asyncio_time])

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("deployment", help="Deployment name")
    parser.add_argument("output_dir", help="Output directory")
    args = parser.parse_args()
   
    filename = f"{args.output_dir}/pod_count.csv"
    idx = 0
    while True:
        status_counts = get_pod_status_counts(args.deployment)
        write_to_csv(args.deployment, status_counts, filename, idx)
        time.sleep(1)
        idx += 1

if __name__ == "__main__":
   main()