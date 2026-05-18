#!/usr/bin/env python3

# Copyright The Kubernetes Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import argparse
import importlib.util
import json
import math
import os
import pathlib
import pickle
import socket
import sys
import time


def _nixl_lib_dirs():
    dirs = []
    for mod_name in ("nixl_cu12", "nixl_cu13"):
        spec = importlib.util.find_spec(mod_name)
        if spec is None or spec.origin is None:
            continue
        package_dir = pathlib.Path(spec.origin).resolve().parent
        for candidate in (
            package_dir.parent / f"{mod_name}.libs",
            package_dir.parent / f".{mod_name}.mesonpy.libs",
            package_dir.parent / f".{mod_name}.mesonpy.libs" / "plugins",
        ):
            if candidate.exists():
                dirs.append(str(candidate))
    return dirs


# nixl's bundled UCX/plugin .so files live in sibling *.libs dirs that aren't
# on the default loader path. ld.so reads LD_LIBRARY_PATH at process start, so
# we prepend the discovered dirs and re-exec before any nixl import.
if not os.environ.get("_NIXL_LD_BOOTSTRAPPED"):
    extra = ":".join(_nixl_lib_dirs())
    if extra:
        os.environ["LD_LIBRARY_PATH"] = (
            f"{extra}:{os.environ.get('LD_LIBRARY_PATH', '')}"
        )
    os.environ["_NIXL_LD_BOOTSTRAPPED"] = "1"
    os.execvp(sys.executable, [sys.executable, __file__, *sys.argv[1:]])

import torch
from nixl import nixl_agent, nixl_agent_config


def parse_args():
    parser = argparse.ArgumentParser(
        description="Two-process NIXL VRAM microbenchmark for Kubernetes pods."
    )
    parser.add_argument("--mode", choices=("target", "initiator"), required=True)
    parser.add_argument("--target-host", default="nixlbench-target")
    parser.add_argument("--port", type=int, default=5555)
    parser.add_argument("--backend", default="UCX")
    parser.add_argument("--op", choices=("READ", "WRITE"), default="WRITE")
    parser.add_argument("--device", default="cuda:0")
    parser.add_argument("--block-size", type=int, default=268435456)
    parser.add_argument("--batch-size", type=int, default=1)
    parser.add_argument("--iters", type=int, default=100)
    parser.add_argument("--warmup-iters", type=int, default=20)
    parser.add_argument("--backend-threads", type=int, default=0)
    parser.add_argument("--progress-thread", action="store_true")
    parser.add_argument("--timeout-seconds", type=float, default=600.0)
    return parser.parse_args()


def percentile(values, pct):
    if not values:
        return 0.0

    sorted_values = sorted(values)
    rank = (len(sorted_values) - 1) * pct / 100.0
    lower = math.floor(rank)
    upper = math.ceil(rank)
    if lower == upper:
        return sorted_values[int(rank)]

    weight = rank - lower
    return sorted_values[lower] * (1.0 - weight) + sorted_values[upper] * weight


def wait_for_tcp(host, port, timeout_seconds):
    deadline = time.monotonic() + timeout_seconds
    last_error = None
    while time.monotonic() < deadline:
        try:
            with socket.create_connection((host, port), timeout=5.0):
                return
        except OSError as err:
            last_error = err
            time.sleep(1.0)

    raise TimeoutError(f"timed out waiting for {host}:{port}: {last_error}")


def resolve_ipv4(host):
    infos = socket.getaddrinfo(host, None, family=socket.AF_INET, type=socket.SOCK_STREAM)
    if not infos:
        raise OSError(f"no IPv4 address found for {host}")
    return infos[0][4][0]


def wait_for_remote_metadata(agent, name, timeout_seconds):
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        if agent.check_remote_metadata(name):
            return
        time.sleep(0.01)

    raise TimeoutError(f"timed out waiting for remote metadata from {name}")


def wait_for_notification(agent, name, timeout_seconds):
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        notifications = agent.get_new_notifs()
        if name in notifications and notifications[name]:
            return notifications[name][0]
        time.sleep(0.01)

    raise TimeoutError(f"timed out waiting for notification from {name}")


def wait_for_done(agent, timeout_seconds):
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        notifications = agent.get_new_notifs()
        for messages in notifications.values():
            if b"BENCH_DONE" in messages:
                return
        time.sleep(0.05)

    raise TimeoutError("timed out waiting for BENCH_DONE")


def wait_for_transfer(agent, handle):
    while True:
        state = agent.check_xfer_state(handle)
        if state == "DONE":
            return
        if state == "ERR":
            raise RuntimeError("NIXL transfer entered ERR state")


def descriptor_tuples(tensor, block_size, batch_size):
    base_addr = tensor.data_ptr()
    device_id = tensor.get_device()
    if device_id < 0:
        device_id = 0

    return [
        (base_addr + (idx * block_size), block_size, device_id)
        for idx in range(batch_size)
    ]


def memory_type(device):
    return "VRAM" if str(device).startswith("cuda") else "DRAM"


def make_agent(name, args, listen_port):
    config = nixl_agent_config(
        enable_prog_thread=args.progress_thread,
        enable_listen_thread=True,
        listen_port=listen_port,
        capture_telemetry=True,
        num_threads=args.backend_threads,
        backends=[args.backend],
    )
    return nixl_agent(name, config)


def allocate_tensor(args):
    if args.block_size <= 0 or args.batch_size <= 0:
        raise ValueError("--block-size and --batch-size must be positive")

    total_bytes = args.block_size * args.batch_size
    device = torch.device(args.device)
    if device.type == "cuda":
        torch.cuda.set_device(device)
    tensor = torch.empty((total_bytes,), dtype=torch.uint8, device=device)
    if device.type == "cuda":
        torch.cuda.synchronize(device)
    return tensor, total_bytes


def run_target(args):
    listen_port = args.port
    agent = make_agent("target", args, listen_port)
    tensor, total_bytes = allocate_tensor(args)
    reg_descs = agent.register_memory(tensor)
    xfer_descs = agent.get_xfer_descs(
        descriptor_tuples(tensor, args.block_size, args.batch_size),
        mem_type=memory_type(tensor.device),
    )

    print(
        json.dumps(
            {
                "event": "target_ready",
                "backend": args.backend,
                "device": str(tensor.device),
                "total_bytes": total_bytes,
                "block_size": args.block_size,
                "batch_size": args.batch_size,
                "listen_port": listen_port,
            },
            sort_keys=True,
        ),
        flush=True,
    )

    try:
        wait_for_remote_metadata(agent, "initiator", args.timeout_seconds)
        agent.send_notif("initiator", pickle.dumps(agent.get_serialized_descs(xfer_descs)))
        wait_for_done(agent, args.timeout_seconds)
    finally:
        agent.deregister_memory(reg_descs)

    print(json.dumps({"event": "target_done"}, sort_keys=True), flush=True)


def run_initiator(args):
    wait_for_tcp(args.target_host, args.port, args.timeout_seconds)
    target_ip = resolve_ipv4(args.target_host)

    agent = make_agent("initiator", args, 0)
    tensor, total_bytes = allocate_tensor(args)
    reg_descs = agent.register_memory(tensor)
    local_descs = agent.get_xfer_descs(
        descriptor_tuples(tensor, args.block_size, args.batch_size),
        mem_type=memory_type(tensor.device),
    )

    try:
        agent.send_local_metadata(target_ip, args.port)
        agent.fetch_remote_metadata("target", target_ip, args.port)
        serialized_remote_descs = pickle.loads(
            wait_for_notification(agent, "target", args.timeout_seconds)
        )
        remote_descs = agent.deserialize_descs(serialized_remote_descs)
        wait_for_remote_metadata(agent, "target", args.timeout_seconds)
        agent.make_connection("target", backends=[args.backend])

        handle = agent.initialize_xfer(
            args.op, local_descs, remote_descs, "target", b"", backends=[args.backend]
        )
        chosen_backend = agent.query_xfer_backend(handle)

        for _ in range(args.warmup_iters):
            state = agent.transfer(handle)
            if state == "ERR":
                raise RuntimeError("failed to post warmup transfer")
            if state != "DONE":
                wait_for_transfer(agent, handle)

        latencies = []
        for _ in range(args.iters):
            start = time.perf_counter_ns()
            state = agent.transfer(handle)
            if state == "ERR":
                raise RuntimeError("failed to post timed transfer")
            if state != "DONE":
                wait_for_transfer(agent, handle)
            latencies.append((time.perf_counter_ns() - start) / 1e9)

        avg_seconds = sum(latencies) / len(latencies)
        summary = {
            "event": "result",
            "backend": chosen_backend,
            "requested_backend": args.backend,
            "op": args.op,
            "device": str(tensor.device),
            "block_size": args.block_size,
            "batch_size": args.batch_size,
            "total_bytes": total_bytes,
            "iters": args.iters,
            "warmup_iters": args.warmup_iters,
            "avg_seconds": avg_seconds,
            "min_seconds": min(latencies),
            "p50_seconds": percentile(latencies, 50),
            "p95_seconds": percentile(latencies, 95),
            "p99_seconds": percentile(latencies, 99),
            "avg_GBps": total_bytes / avg_seconds / 1e9,
        }
        print("RESULT " + json.dumps(summary, sort_keys=True), flush=True)

        agent.send_notif("target", b"BENCH_DONE")
        agent.release_xfer_handle(handle)
        agent.remove_remote_agent("target")
        agent.invalidate_local_metadata(target_ip, args.port)
    finally:
        agent.deregister_memory(reg_descs)


def main():
    args = parse_args()
    try:
        if args.mode == "target":
            run_target(args)
        else:
            run_initiator(args)
    except Exception as err:
        print(f"ERROR {err}", file=sys.stderr, flush=True)
        raise


if __name__ == "__main__":
    main()
