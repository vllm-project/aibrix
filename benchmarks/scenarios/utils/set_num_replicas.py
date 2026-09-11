import argparse
from kubernetes import client, config
import sys

def scale_deployment(deployment_name, replicas, kube_context):
    try:
        config.load_kube_config(context=kube_context)
        apps_v1 = client.AppsV1Api()
        apps_v1.patch_namespaced_deployment(
            name=deployment_name,
            namespace="default",
            body={"spec": {"replicas": replicas}}
        )
    except Exception as e:
        print(f"Error: {e}")
        sys.exit(1)

if __name__ == "__main__":
   parser = argparse.ArgumentParser()
   parser.add_argument("--deployment", required=True)
   parser.add_argument("--replicas", type=int, required=True)
   parser.add_argument("--context", required=True)
   args = parser.parse_args()
   
   scale_deployment(args.deployment, args.replicas, args.context)
   print(f"Deployment {args.deployment} scaled to {args.replicas} replica(s)")