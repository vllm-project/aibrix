#!/usr/bin/env python3
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

API_VERSION = "routing.aibrix.ai/v1alpha1"
MEDIA_TYPE = "application/vnd.aibrix.external-routing+json;version=v1alpha1"


def decide(document, preferred_zone=None, premium_model="premium-model"):
    if document.get("apiVersion") != API_VERSION or document.get("kind") != "ReplicaSelectionRequest":
        raise ValueError("unsupported request envelope")
    metadata = document.get("metadata") or {}
    spec = document.get("spec") or {}
    request_id = metadata.get("requestId")
    candidates = sorted(spec.get("candidates") or [], key=lambda item: item.get("id", ""))
    if not request_id or not candidates:
        raise ValueError("requestId and candidates are required")

    eligible = candidates
    if spec.get("model") == premium_model:
        eligible = [
            item for item in eligible
            if (item.get("attributes") or {}).get("routing.example.com/accelerator-class") == "h100"
        ]
    if preferred_zone:
        zonal = [
            item for item in eligible
            if (item.get("attributes") or {}).get("topology.kubernetes.io/zone") == preferred_zone
        ]
        if zonal:
            eligible = zonal
    if not eligible:
        status = {"decision": "NoDecision", "reason": "NoApplicablePolicy"}
    else:
        winner = eligible[0]
        ports = winner.get("ports") or []
        if not ports:
            raise ValueError("candidate ports are required")
        status = {
            "decision": "Selected",
            "target": {"id": winner["id"], "port": sorted(ports)[0]},
        }
    return {
        "apiVersion": API_VERSION,
        "kind": "ReplicaSelectionResponse",
        "metadata": {"requestId": request_id},
        "status": status,
    }


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/v1alpha1/select":
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length <= 0 or length > 262144:
                raise ValueError("invalid body size")
            request = json.loads(self.rfile.read(length))
            response = decide(request, os.getenv("PREFERRED_ZONE"))
            body = json.dumps(response, separators=(",", ":")).encode()
            self.send_response(200)
            self.send_header("Content-Type", MEDIA_TYPE)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        except (ValueError, json.JSONDecodeError) as error:
            body = json.dumps({"title": str(error), "status": 400}).encode()
            self.send_response(400)
            self.send_header("Content-Type", "application/problem+json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    def log_message(self, _format, *_args):
        return


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()

