#!/usr/bin/env bats

# Reproduces kubernetes-sigs/dranet#310: attaching a Pod's devices in the NRI
# RunPodSandbox hook takes longer than the container runtime's NRI request
# timeout (containerd plugin_request_timeout, 2s by default). The end-to-end
# build of the driver (-tags e2e, see pkg/driver/attach_e2e.go) slows every
# attach down through DRANET_E2E_ATTACH_DELAY so six dummy interfaces do not
# fit in one request, and the tests check how the driver and the runtime
# react under each setting. The dranet pod on the node must never be
# disconnected or restarted, and no Pod may run with a subset of its devices.

load 'test_helper/bats-support/load'
load 'test_helper/bats-assert/load'

NODE="dranet-test-cluster-worker"
POD="pod-slow-attach"
# Six devices at 700ms each need ~4.2s: two fit in a 2s request (200ms is
# reserved to answer before the deadline), so RunPodSandbox returns with 2/6
# attached and the attach job running.
ATTACH_DELAY="700ms"
NUM_DEVICES=6

setup_file() {
  export BATS_TEST_TIMEOUT=300
  set_test_args
  # With the image present, the kubelet creates the first container right
  # after the sandbox, while the attach job is still running.
  docker exec "$NODE" crictl pull registry.k8s.io/e2e-test-images/agnhost:2.54
}

teardown_file() {
  kubectl delete -f "$BATS_TEST_DIRNAME"/manifests/deviceclass.yaml --ignore-not-found || true
  ATTACH_DELAY=""
  set_test_args || true
  # Restore the containerd config if the last test did not get to it.
  if docker exec "$NODE" test -f /etc/containerd/config.toml.bak; then
    docker exec "$NODE" mv /etc/containerd/config.toml.bak /etc/containerd/config.toml
    docker exec "$NODE" systemctl restart containerd
    sleep 10
    wait_for_dranet || true
  fi
}

setup() {
  kubectl apply -f "$BATS_TEST_DIRNAME"/manifests/deviceclass.yaml
  kubectl apply -f "$BATS_TEST_DIRNAME"/manifests/slow_attach_template.yaml
  for i in $(seq 0 $((NUM_DEVICES - 1))); do
    docker exec "$NODE" bash -c "ip link add slow$i type dummy && ip link set up dev slow$i"
  done
  # Let the inventory publish the new devices before the Pod is scheduled.
  sleep 3
  DRANET_POD=$(dranet_pod)
  DRANET_RESTARTS=$(dranet_restarts)
}

teardown() {
  if [[ -z "$BATS_TEST_COMPLETED" || "$BATS_TEST_COMPLETED" -ne 1 ]] && [[ -z "$BATS_TEST_SKIPPED" ]]; then
    dump_debug_info
  fi
  kubectl delete -f "$BATS_TEST_DIRNAME"/manifests/pod_slow_attach.yaml --ignore-not-found --wait=true --timeout=60s || true
  kubectl delete -f "$BATS_TEST_DIRNAME"/manifests/pod_slow_attach_multi.yaml --ignore-not-found --wait=true --timeout=60s || true
  kubectl delete -f "$BATS_TEST_DIRNAME"/manifests/slow_attach_template.yaml --ignore-not-found || true
  docker exec "$NODE" bash -c '
    for dev in $(ip -br link show type dummy | awk "{print \$1}"); do
      ip link delete "$dev" || true
    done
  '
  # The inventory is rate limited; give it time to drop the deleted devices.
  sleep 5
}

# ---- helpers ----

dranet_pod() {
  kubectl -n kube-system get pods -l k8s-app=dranet --field-selector spec.nodeName="$NODE" -o jsonpath='{.items[0].metadata.name}'
}

dranet_restarts() {
  kubectl -n kube-system get pod "$DRANET_POD" -o jsonpath='{.status.containerStatuses[0].restartCount}'
}

wait_for_dranet() {
  kubectl rollout status daemonset/dranet -n kube-system --timeout=120s
  kubectl wait --for=condition=ready pods --namespace=kube-system -l k8s-app=dranet --timeout=120s
}

# set_test_args replaces the flags this file manages on the dranet DaemonSet
# with the given ones and sets the attach delay of the end-to-end build from
# ATTACH_DELAY (empty removes it), so a failed test cannot leave either behind.
set_test_args() {
  local extra
  extra=$(printf '%s\n' "$@" | jq -R . | jq -sc .)
  local spec args env
  spec=$(kubectl -n kube-system get daemonset dranet -o json)
  args=$(jq -c --argjson extra "$extra" '[.spec.template.spec.containers[0].args[] | select((startswith("--device-attach-timeout") or startswith("--hook-path")) | not)] + ($extra | map(select(. != "")))' <<<"$spec")
  env=$(jq -c --arg delay "$ATTACH_DELAY" '[(.spec.template.spec.containers[0].env // [])[] | select(.name != "DRANET_E2E_ATTACH_DELAY")] + (if $delay == "" then [] else [{"name": "DRANET_E2E_ATTACH_DELAY", "value": $delay}] end)' <<<"$spec")
  kubectl patch daemonset dranet -n kube-system --type=json -p='[
    {"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": '"$args"'},
    {"op": "add", "path": "/spec/template/spec/containers/0/env", "value": '"$env"'}
  ]'
  wait_for_dranet
}

pod_events() {
  # By UID: the tests reuse the pod name and events outlive the pod.
  local uid
  uid=$(kubectl get pod "$POD" -o jsonpath='{.metadata.uid}')
  kubectl get events --field-selector involvedObject.uid="$uid" -o jsonpath='{range .items[*]}{.reason}{": "}{.message}{"\n"}{end}'
}

# The driver must have stayed connected: no restart and no NRI disconnect.
assert_dranet_stayed_connected() {
  assert_equal "$(dranet_restarts)" "$DRANET_RESTARTS"
  run kubectl -n kube-system logs "$DRANET_POD"
  refute_output --partial "NRI plugin closed"
}

assert_pod_has_all_devices() {
  for i in $(seq 0 $((NUM_DEVICES - 1))); do
    run kubectl exec "$POD" -- ip link show "slow$i"
    assert_success
  done
  run kubectl get resourceclaims -o jsonpath='{.items[0].status.devices[*].conditions[*].reason}'
  assert_success
  assert_equal "$(grep -o NetworkDeviceReady <<<"$output" | wc -l)" "$NUM_DEVICES"
}

dump_debug_info() {
  echo "--- Test failed. Dumping debug information ---"
  echo "--- Pod ---"
  kubectl describe pod "$POD" || true
  echo "--- Pod events ---"
  pod_events || true
  echo "--- ResourceClaims ---"
  kubectl get resourceclaims -o yaml || true
  echo "--- dranet logs on $NODE ---"
  kubectl -n kube-system logs "$DRANET_POD" --tail=200 || true
  echo "--- hook binary and socket on $NODE ---"
  docker exec "$NODE" ls -l /opt/dranet/bin/dranet-hook /var/run/dranet/hook.sock || true
  echo "--- containerd NRI config on $NODE ---"
  docker exec "$NODE" grep -A3 'io.containerd.nri.v1.nri' /etc/containerd/config.toml || true
  echo "--- End of debug information ---"
}

# ---- tests ----

@test "the hook binary and socket are installed on the host" {
  run docker exec "$NODE" test -x /opt/dranet/bin/dranet-hook
  assert_success
  run docker exec "$NODE" test -S /var/run/dranet/hook.sock
  assert_success
}

@test "slow device attach continues after RunPodSandbox and the containers wait for it through the OCI hook" {
  kubectl apply -f "$BATS_TEST_DIRNAME"/manifests/pod_slow_attach.yaml
  kubectl wait --for=create pod/"$POD" --timeout=30s
  # 2/6 at sandbox creation; the job attaches the rest in ~3s while the
  # container's createRuntime hook waits for it. No kubelet retry is needed.
  kubectl wait --for=condition=ready pod/"$POD" --timeout=120s

  assert_pod_has_all_devices
  assert_dranet_stayed_connected

  run pod_events
  assert_output --partial "NetworkDeviceAttachDeferred"
  assert_output --partial "attached 2/6 network devices"
  refute_output --partial "NetworkDeviceAttachTimeout"
  refute_output --partial "FailedCreatePodSandBox"
  # The hook held the container until the devices were attached: no failed
  # container creation was needed.
  refute_output --partial "Failed: Error"
  run kubectl get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}'
  assert_output "0"

  # The container's OCI spec carries the hook with the Pod and its devices in
  # the environment, as the hook contract documents.
  run kubectl -n kube-system logs "$DRANET_POD"
  assert_output --regexp 'Injected the device attach hook.*container="agnhost"'
  container_id=$(kubectl get pod "$POD" -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's#containerd://##')
  run docker exec "$NODE" crictl inspect "$container_id"
  assert_success
  spec="$output"
  run jq -r '.info.runtimeSpec.hooks.createRuntime[0].path' <<<"$spec"
  assert_output "/opt/dranet/bin/dranet-hook"
  run jq -r '.info.runtimeSpec.hooks.createRuntime[0].env[]' <<<"$spec"
  assert_output --partial "DRANET_POD_UID=$(kubectl get pod "$POD" -o jsonpath='{.metadata.uid}')"
  assert_output --partial "DRANET_POD_NAME=$POD"
  assert_output --partial "DRANET_CONTAINER_NAME=agnhost"
  assert_output --partial "DRANET_NETNS=/var/run/netns/"
  assert_output --partial "DRANET_SOCKET=/var/run/dranet/hook.sock"
  devices=$(jq -r '.info.runtimeSpec.hooks.createRuntime[0].env[] | select(startswith("DRANET_DEVICES=")) | sub("^DRANET_DEVICES="; "")' <<<"$spec")
  assert_equal "$(jq 'length' <<<"$devices")" "$NUM_DEVICES"
  assert_equal "$(jq -r '.[0].host.name' <<<"$devices")" "slow0"
  assert_equal "$(jq -r '.[0].claim.namespace' <<<"$devices")" "default"
}

@test "in a pod with init, sidecar and app containers only the first container waits and a restart is not gated" {
  kubectl apply -f "$BATS_TEST_DIRNAME"/manifests/pod_slow_attach_multi.yaml
  kubectl wait --for=create pod/"$POD" --timeout=30s
  # The app container exits once and the kubelet restarts it (~10s backoff);
  # wait for the restart so the pod is stable before inspecting it.
  kubectl wait --for=jsonpath='{.status.containerStatuses[0].restartCount}'=1 pod/"$POD" --timeout=120s
  kubectl wait --for=condition=ready pod/"$POD" --timeout=120s

  assert_pod_has_all_devices
  assert_dranet_stayed_connected

  # The init container, first to be created, already saw all the devices.
  run kubectl exec "$POD" -c app -- cat /shared/init-devices
  assert_success
  assert_output "$NUM_DEVICES"

  # The hook was injected into the init container only; the sidecar, the
  # app and its restart were created without it. Filter the driver log by
  # this pod: the dranet pod has served earlier tests.
  pod_uid=$(kubectl get pod "$POD" -o jsonpath='{.metadata.uid}')
  run kubectl -n kube-system logs "$DRANET_POD"
  assert_success
  injected=$(grep 'Injected the device attach hook' <<<"$output" | grep -c "podUID=\"$pod_uid\"" || true)
  assert_equal "$injected" "1"
  assert_output --regexp "Injected the device attach hook.*podUID=\"$pod_uid\".*container=\"init\""

  run kubectl get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}'
  assert_output "1"
  run pod_events
  assert_output --partial "NetworkDeviceAttachDeferred"
  refute_output --partial "Failed: Error"
}

@test "without the hook a slow attach fails container creation until the devices are attached, then the pod starts" {
  set_test_args "--hook-path="
  DRANET_POD=$(dranet_pod)
  DRANET_RESTARTS=$(dranet_restarts)

  kubectl apply -f "$BATS_TEST_DIRNAME"/manifests/pod_slow_attach.yaml
  kubectl wait --for=create pod/"$POD" --timeout=30s
  # The first container creation fails while the job runs; the kubelet
  # retries ~10s later, by then the devices are attached.
  kubectl wait --for=condition=ready pod/"$POD" --timeout=120s

  assert_pod_has_all_devices
  assert_dranet_stayed_connected
  run pod_events
  assert_output --partial "NetworkDeviceAttachDeferred"
  assert_output --partial "the kubelet will retry"
  refute_output --partial "FailedCreatePodSandBox"

  set_test_args
}

@test "with --device-attach-timeout=0 a slow attach fails the sandbox instead of starting the pod with missing devices" {
  set_test_args "--device-attach-timeout=0"
  DRANET_POD=$(dranet_pod)
  DRANET_RESTARTS=$(dranet_restarts)

  kubectl apply -f "$BATS_TEST_DIRNAME"/manifests/pod_slow_attach.yaml
  kubectl wait --for=create pod/"$POD" --timeout=30s
  # Every sandbox attempt attaches 2/6 and fails before the runtime deadline;
  # the kubelet recreates the sandbox and the pod never runs.
  sleep 30

  run kubectl get pod "$POD" -o jsonpath='{.status.phase}'
  assert_output "Pending"
  run kubectl get pod "$POD" -o jsonpath='{.status.containerStatuses[*].state.running}'
  assert_output ""
  run pod_events
  assert_output --partial "NetworkDeviceAttachTimeout"
  assert_output --partial "FailedCreatePodSandBox"
  assert_output --partial "attached 2/6 network devices"
  refute_output --partial "NetworkDeviceAttachDeferred"
  assert_dranet_stayed_connected

  # The devices moved into the failed sandboxes are back on the host once the
  # kubelet stops recreating the sandbox.
  kubectl delete pod "$POD" --wait=true --timeout=60s
  for i in $(seq 0 $((NUM_DEVICES - 1))); do
    run docker exec "$NODE" ip link show "slow$i"
    assert_success
  done

  set_test_args
}

@test "a larger containerd plugin_request_timeout attaches all devices at sandbox creation" {
  docker exec "$NODE" cp /etc/containerd/config.toml /etc/containerd/config.toml.bak
  docker exec "$NODE" bash -c 'cat >> /etc/containerd/config.toml <<EOF

[plugins."io.containerd.nri.v1.nri"]
  plugin_request_timeout = "10s"
EOF'
  docker exec "$NODE" systemctl restart containerd
  # The driver may lose its NRI connection while containerd restarts; give it
  # time to settle before taking the pod as a baseline.
  sleep 10
  wait_for_dranet
  DRANET_POD=$(dranet_pod)

  kubectl apply -f "$BATS_TEST_DIRNAME"/manifests/pod_slow_attach.yaml
  kubectl wait --for=create pod/"$POD" --timeout=30s
  kubectl wait --for=condition=ready pod/"$POD" --timeout=120s

  assert_pod_has_all_devices
  run pod_events
  refute_output --partial "NetworkDeviceAttachDeferred"
  refute_output --partial "NetworkDeviceAttachTimeout"

  docker exec "$NODE" mv /etc/containerd/config.toml.bak /etc/containerd/config.toml
  docker exec "$NODE" systemctl restart containerd
  sleep 10
  wait_for_dranet
}
