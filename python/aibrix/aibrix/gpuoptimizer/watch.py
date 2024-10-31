import threading
from kubernetes import client, config, watch
import time

def watch_deployments(namespace):
    """Watches for deployment events and can be stopped externally."""
    try:
        try:
            config.load_incluster_config()
        except Exception as e:
            config.load_kube_config(config_file="~/.kube/config")
        apps_v1 = client.AppsV1Api()
        w = watch.Watch()

        # Start the watch in a separate thread
        def watch_loop():
            try:
                for event in w.stream(apps_v1.list_namespaced_deployment, namespace=namespace,timeout_seconds=10):
                    pass
                        # ... process event ...
            except Exception as e:
                print(f"Watch loop exited: {e}")  # Log the exception

        watch_thread = threading.Thread(target=watch_loop)
        watch_thread.start()

        time.sleep(1)

        # ... do other things ...

        # Stop the watch from outside the loop
        w.stop() 

        # ... continue with other tasks ...

    except Exception as e:
        print(f"Error: {e}")

if __name__ == "__main__":
    namespace = "my-namespace"
    watch_deployments(namespace)