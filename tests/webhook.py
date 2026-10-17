#!/usr/bin/env python3

import ctypes
import json
import logging
import os
from http.server import BaseHTTPRequestHandler, HTTPServer
import argparse

logging.basicConfig(level=logging.INFO)

# The runtime provider marks the claimed interface, inside the Pod's network
# namespace, with this value in a sysctl the Pod can read: it proves it
# ran in the right namespace before the Pod's first container started.
RUNTIME_PROVIDER_TAG = "4242"
CLONE_NEWNET = 0x40000000

# ConfigurePod calls per Pod UID, to fail the first attempt of the Pods that
# test the kubelet's retry.
configure_pod_calls = {}


def setns(fd):
    if hasattr(os, "setns"):
        os.setns(fd, CLONE_NEWNET)
        return
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.setns(fd, CLONE_NEWNET) != 0:
        errno = ctypes.get_errno()
        raise OSError(errno, os.strerror(errno))


def tag_interface_in_netns(netns, ifname):
    """Sets the marker sysctl of ifname in the network namespace at netns.

    /proc/sys/net is resolved against the network namespace of the calling
    process, so entering the namespace is enough to reach the Pod's sysctls.
    """
    host = os.open("/proc/self/ns/net", os.O_RDONLY)
    try:
        target = os.open(netns, os.O_RDONLY)
        try:
            setns(target)
            try:
                with open("/proc/sys/net/ipv4/conf/%s/tag" % ifname, "w") as f:
                    f.write(RUNTIME_PROVIDER_TAG)
            finally:
                setns(host)
        finally:
            os.close(target)
    finally:
        os.close(host)


def configure_pod(req):
    """Validates the request and configures the Pod. Returns (status, body)."""
    pod = req.get("pod", {})
    for field in ("namespace", "name", "uid"):
        if not pod.get(field):
            return 400, "pod.%s is missing" % field
    netns = req.get("netns", "")
    if not netns or not os.path.exists(netns):
        return 400, "netns %r does not exist on the node" % netns
    claims = req.get("claims", [])
    if not claims:
        return 400, "no claims"
    for claim in claims:
        metadata = claim.get("claim", {}).get("metadata", {})
        if not metadata.get("uid") or not metadata.get("name"):
            return 400, "claim without identity: %r" % claim.get("claim")
        if not claim.get("devices"):
            return 400, "claim %s without devices" % metadata.get("name")

    name = pod["name"]
    calls = configure_pod_calls.get(pod["uid"], 0) + 1
    configure_pod_calls[pod["uid"]] = calls
    if "fail" in name:
        return 500, "pod rejected by the python webhook"
    if "retry" in name and calls == 1:
        return 503, "pod not ready yet, retry (attempt %d)" % calls

    for claim in claims:
        for device in claim["devices"]:
            ifname = device.get("config", {}).get("interface", {}).get("name")
            if not ifname:
                continue
            tag_interface_in_netns(netns, ifname)
            logging.info("runtime provider: tagged %s in %s for pod %s/%s (attempt %d)",
                         ifname, netns, pod["namespace"], name, calls)
    return 200, json.dumps({"attempt": calls})


class WebhookHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/health':
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps({
                "cloudProvider": True,
                "profileProvider": True,
                "runtimeProvider": True
            }).encode('utf-8'))
        else:
            self.send_response(404)
            self.end_headers()

    def do_POST(self):
        logging.info("POST request to %s", self.path)
        content_length = int(self.headers.get('Content-Length', 0))
        post_data = self.rfile.read(content_length)
        logging.info("Body: %s", post_data.decode('utf-8'))

        try:
            req_json = json.loads(post_data.decode('utf-8'))
        except Exception:
            req_json = {}

        if self.path == '/GetDeviceConfig':
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            
            # Cloud intent sets the MTU only for dummy1
            resp = {}
            if req_json.get("name") == "dummy1":
                resp = {
                    "interface": {
                        "mtu": 1450
                    }
                }
            self.wfile.write(json.dumps(resp).encode('utf-8'))
        elif self.path == '/GetProfileConfig':
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            
            # User intent profile sets the address for python-profile
            resp = {}
            config_obj = req_json.get("config", {})
            profile_name = config_obj.get("profile")
            if profile_name == "python-profile":
                resp = {
                    "interface": {
                        "addresses": ["10.200.200.200/24"]
                    }
                }
            self.wfile.write(json.dumps(resp).encode('utf-8'))
        elif self.path == '/ReleaseProfileConfig':
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            self.wfile.write(b'{}')
        elif self.path == '/ConfigurePod':
            try:
                status, body = configure_pod(req_json)
            except Exception as e:  # report what went wrong to DRANET
                logging.exception("ConfigurePod failed")
                status, body = 500, "ConfigurePod error: %s" % e
            self.send_response(status)
            self.send_header('Content-type', 'application/json' if status == 200 else 'text/plain')
            self.end_headers()
            self.wfile.write(body.encode('utf-8'))
        elif self.path == '/GetDeviceAttributes':
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            
            # Python web hook expose some custom attribute only for dummy1
            resp = {}
            if req_json.get("name") == "dummy1":
                resp = {"dra.net/webhook_attr": {"string": "python"}}
            self.wfile.write(json.dumps(resp).encode('utf-8'))
        else:
            self.send_response(404)
            self.end_headers()

def run(server_class=HTTPServer, handler_class=WebhookHandler, port=8080):
    server_address = ('', port)
    httpd = server_class(server_address, handler_class)
    logging.info("Starting httpd on port %d...", port)
    httpd.serve_forever()

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument('--port', type=int, default=8080)
    args = parser.parse_args()
    run(port=args.port)
