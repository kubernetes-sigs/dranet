#!/usr/bin/env bats

load 'test_helper/bats-support/load'
load 'test_helper/bats-assert/load'

setup_file() {
  export BATS_TEST_TIMEOUT=300

  # Create ConfigMap with the python webhook script
  kubectl --context kind-dranet-test-cluster create configmap python-webhook-script -n kube-system --from-file=webhook.py="$BATS_TEST_DIRNAME"/webhook.py
  
  # Deploy the python webhook daemonset
  kubectl --context kind-dranet-test-cluster apply -f "$BATS_TEST_DIRNAME"/../tests/manifests/python_webhook_daemonset.yaml
  kubectl --context kind-dranet-test-cluster rollout status daemonset/python-webhook -n kube-system --timeout=120s

  # Patch daemonset to use the webhook cloud provider, profile provider, and runtime provider
  kubectl --context kind-dranet-test-cluster patch daemonset dranet -n kube-system --type=json -p='[
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--cloud-provider-hint=webhook"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--profile-provider=webhook"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--webhook-url=http://127.0.0.1:8081"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--feature-gates=RuntimeProvider=true"}
  ]'
  
  if ! kubectl --context kind-dranet-test-cluster rollout status daemonset/dranet -n kube-system --timeout=120s; then
    echo "Rollout failed! Printing debug info:" >&3
    kubectl --context kind-dranet-test-cluster get pods -n kube-system -l app=dranet >&3
    kubectl --context kind-dranet-test-cluster describe pods -n kube-system -l app=dranet >&3
    kubectl --context kind-dranet-test-cluster logs -n kube-system -l app=dranet --tail=100 >&3
    kubectl --context kind-dranet-test-cluster logs -n kube-system -l app=python-webhook --tail=100 >&3
    exit 1
  fi
  
  # Delete dranet pods to restart them with new config
  kubectl --context kind-dranet-test-cluster delete pods -n kube-system -l app=dranet
  kubectl --context kind-dranet-test-cluster wait --for=create --for=condition=ready pods --namespace=kube-system -l app=dranet --timeout=120s
}

teardown_file() {
  kubectl --context kind-dranet-test-cluster patch daemonset dranet -n kube-system --type=json -p='[
    {"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["/dranet", "--v=4", "--hostname-override=$(NODE_NAME)"]}
  ]' || true
  kubectl --context kind-dranet-test-cluster rollout status daemonset/dranet -n kube-system --timeout=120s
  
  kubectl --context kind-dranet-test-cluster delete -f "$BATS_TEST_DIRNAME"/../tests/manifests/python_webhook_daemonset.yaml || true
  kubectl --context kind-dranet-test-cluster delete configmap python-webhook-script -n kube-system || true
}

teardown() {
  # Clean up even when an assertion failed, so a leftover pod or claim does
  # not break the tests that run after this one.
  kubectl --context kind-dranet-test-cluster delete pod pod-python-fail pod-python-retry --ignore-not-found --wait=true || true
  kubectl --context kind-dranet-test-cluster delete -f "$BATS_TEST_DIRNAME"/../tests/manifests/python_webhook_pod.yaml --ignore-not-found --wait=true || true
  kubectl --context kind-dranet-test-cluster delete -f "$BATS_TEST_DIRNAME"/../tests/manifests/deviceclass.yaml --ignore-not-found || true
  docker exec dranet-test-cluster-worker bash -c "ip link delete dev dummy1 || true"
}

# prepare_python_device creates the dummy interface the python webhook
# recognizes and waits for DRANET to publish it with the webhook's attribute.
prepare_python_device() {
  local NODE_NAME="dranet-test-cluster-worker"

  docker exec "$NODE_NAME" bash -c "ip link add dummy1 type dummy || true"
  docker exec "$NODE_NAME" bash -c "ip link set up dev dummy1 || true"

  # Apply DeviceClass to trigger dranet hardware matching
  kubectl --context kind-dranet-test-cluster apply -f "$BATS_TEST_DIRNAME"/../tests/manifests/deviceclass.yaml

  # Wait for the interface to be discovered and validate the custom attribute from webhook
  local max_retries=15
  local i=0
  local attr_val=""
  while [ $i -lt $max_retries ]; do
    attr_val=$(kubectl --context kind-dranet-test-cluster get resourceslices --field-selector spec.nodeName="$NODE_NAME" -o jsonpath='{.items[0].spec.devices[?(@.name=="dummy1")].attributes.dra\.net\/webhook_attr.string}' || true)
    if [[ "$attr_val" == "python" ]]; then
      break
    fi
    sleep 2
    i=$((i+1))
  done
  
  if [[ "$attr_val" != "python" ]]; then
    echo "ResourceSlice was not updated with python attribute after $max_retries retries!" >&3
    kubectl --context kind-dranet-test-cluster get resourceslices --field-selector spec.nodeName="$NODE_NAME" -o yaml >&3
  fi
  
  run echo "$attr_val"
  assert_output "python"
}

# create_python_pod creates the test Pod under another name: the python
# webhook decides how it configures the Pod from the Pod name.
create_python_pod() {
  local name="$1"
  sed "s/name: pod-python-webhook/name: $name/" "$BATS_TEST_DIRNAME"/../tests/manifests/python_webhook_pod.yaml |
    kubectl --context kind-dranet-test-cluster apply -f -
}

# runtime_hook_count returns how many createRuntime hooks run the DRANET
# executable in the container's OCI spec.
runtime_hook_count() {
  local container_id
  container_id=$(kubectl --context kind-dranet-test-cluster get pod "$1" -o jsonpath="{.status.containerStatuses[?(@.name==\"$2\")].containerID}" | sed 's#containerd://##')
  docker exec dranet-test-cluster-worker crictl inspect "$container_id" |
    jq '[.info.runtimeSpec.hooks.createRuntime // [] | .[] | select(.args[1] == "oci-hook")] | length'
}

@test "validate python webhook configuration integration" {
  local NODE_NAME="dranet-test-cluster-worker"
  prepare_python_device

  # Create a pod requesting the device
  kubectl --context kind-dranet-test-cluster apply -f "$BATS_TEST_DIRNAME"/../tests/manifests/python_webhook_pod.yaml

  kubectl --context kind-dranet-test-cluster wait --for=condition=ready pod/pod-python-webhook --timeout=120s
  
  # Validate pod has IP and MTU from the python webhook (10.200.200.200 and MTU 1450)
  run kubectl --context kind-dranet-test-cluster exec pod-python-webhook -c app -- ip -4 addr show
  assert_output --partial "10.200.200.200"

  run kubectl --context kind-dranet-test-cluster exec pod-python-webhook -c app -- ip link show dummy1
  assert_output --partial "mtu 1450"

  # The runtime provider ran in the pod's network namespace, after
  # DRANET attached the device and before the first container started: the
  # marker it set on the interface was there when the process started.
  run kubectl --context kind-dranet-test-cluster exec pod-python-webhook -c app -- cat /tmp/tag-at-start
  assert_output "4242"
  run kubectl --context kind-dranet-test-cluster exec pod-python-webhook -c second -- cat /proc/sys/net/ipv4/conf/dummy1/tag
  assert_output "4242"

  # The hook is the DRANET executable, reached through /proc/<pid>/exe of the
  # daemon, in the OCI spec of the first container and only there: the hook
  # runs once per pod.
  run runtime_hook_count pod-python-webhook app
  assert_output "1"
  run runtime_hook_count pod-python-webhook second
  assert_output "0"
  first_id=$(kubectl --context kind-dranet-test-cluster get pod pod-python-webhook -o jsonpath='{.status.containerStatuses[?(@.name=="app")].containerID}' | sed 's#containerd://##')
  run docker exec "$NODE_NAME" crictl inspect "$first_id"
  assert_success
  run jq -r '.info.runtimeSpec.hooks.createRuntime[] | select(.args[1] == "oci-hook") | .path' <<<"$output"
  assert_output --regexp '^/proc/[0-9]+/exe$'

  # The webhook was called once for the pod, with its network namespace.
  run kubectl --context kind-dranet-test-cluster logs -n kube-system -l app=python-webhook --tail=-1
  assert_output --partial "runtime provider: tagged dummy1 in /var/run/netns/"
  assert_output --partial "for pod default/pod-python-webhook (attempt 1)"

  # DRANET reports the hook.
  run kubectl --context kind-dranet-test-cluster logs -n kube-system -l app=dranet --tail=-1
  assert_output --partial "Runtime provider configured the pod"
}

@test "runtime provider failure blocks the containers and is reported" {
  prepare_python_device
  create_python_pod pod-python-fail

  # No container process starts while the provider rejects the pod, and the
  # provider's reason reaches the pod's events through the runtime.
  local attempt
  for attempt in $(seq 1 60); do
    if kubectl --context kind-dranet-test-cluster get events --field-selector involvedObject.name=pod-python-fail -o jsonpath='{.items[*].message}' | grep -q "rejected by the python webhook"; then
      break
    fi
    sleep 2
  done
  run kubectl --context kind-dranet-test-cluster get events --field-selector involvedObject.name=pod-python-fail -o jsonpath='{.items[*].message}'
  assert_output --partial "rejected by the python webhook"
  # Both the runtime (through the hook's stderr) and DRANET report it.
  run kubectl --context kind-dranet-test-cluster get events --field-selector involvedObject.name=pod-python-fail -o jsonpath='{.items[*].reason}'
  assert_output --partial "RuntimeProviderFailed"
  run kubectl --context kind-dranet-test-cluster get pod pod-python-fail -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
  assert_output "False"
  run kubectl --context kind-dranet-test-cluster get pod pod-python-fail -o json
  assert_success
  run jq '[.status.containerStatuses[]? | select(.state.running != null or .state.terminated != null)] | length' <<<"$output"
  assert_output "0"
}

@test "runtime provider transient failure is retried by the kubelet" {
  prepare_python_device
  create_python_pod pod-python-retry

  # The first attempt fails; the kubelet retries the container, which
  # carries the hook again, and the provider succeeds the second time.
  kubectl --context kind-dranet-test-cluster wait --for=condition=ready pod/pod-python-retry --timeout=180s
  run kubectl --context kind-dranet-test-cluster exec pod-python-retry -c app -- cat /tmp/tag-at-start
  assert_output "4242"
  run kubectl --context kind-dranet-test-cluster logs -n kube-system -l app=python-webhook --tail=-1
  assert_output --partial "for pod default/pod-python-retry (attempt 2)"
  run kubectl --context kind-dranet-test-cluster get events --field-selector involvedObject.name=pod-python-retry -o jsonpath='{.items[*].message}'
  assert_output --partial "retry (attempt 1)"
  # The hook ran until it succeeded once for the pod and not after: the
  # kubelet may start the second container before it retries the first, so
  # either one carried the successful hook, and the other carries none.
  app_hooks=$(runtime_hook_count pod-python-retry app)
  second_hooks=$(runtime_hook_count pod-python-retry second)
  assert_equal "$((app_hooks + second_hooks))" "1"
}
