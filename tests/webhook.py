#!/usr/bin/env python3

import json
import logging
from http.server import BaseHTTPRequestHandler, HTTPServer
import argparse

logging.basicConfig(level=logging.INFO)

class WebhookHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/health':
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps({
                "cloudProvider": True,
                "profileProvider": True,
                "runtimeHook": True
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
        elif self.path == '/GetRuntimeHook':
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()

            # The python-profile runs a post-configuration hook on the node
            # when the pod's containers are created, with opaque data.
            resp = None
            config_obj = req_json.get("config", {})
            if config_obj.get("profile") == "python-profile":
                resp = {
                    "path": "/opt/dranet/bin/python-post-hook.sh",
                    "timeoutSeconds": 5,
                    "data": {"vendor": "python", "rail": 1}
                }
            self.wfile.write(json.dumps(resp).encode('utf-8'))
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
