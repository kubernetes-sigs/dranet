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

  # Patch daemonset to use the webhook cloud provider and profile provider
  kubectl --context kind-dranet-test-cluster patch daemonset dranet -n kube-system --type=json -p='[
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--cloud-provider-hint=webhook"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--profile-provider=webhook"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--webhook-url=http://127.0.0.1:8081"}
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

@test "validate python webhook configuration integration" {
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

  # Create a pod requesting the device
  kubectl --context kind-dranet-test-cluster apply -f "$BATS_TEST_DIRNAME"/../tests/manifests/python_webhook_pod.yaml

  kubectl --context kind-dranet-test-cluster wait --for=condition=ready pod/pod-python-webhook --timeout=120s
  
  # Validate pod has IP and MTU from the python webhook (10.200.200.200 and MTU 1450)
  run kubectl --context kind-dranet-test-cluster exec pod-python-webhook -- ip -4 addr show
  assert_output --partial "10.200.200.200"

  run kubectl --context kind-dranet-test-cluster exec pod-python-webhook -- ip link show dummy1
  assert_output --partial "mtu 1450"
  
  kubectl --context kind-dranet-test-cluster delete -f "$BATS_TEST_DIRNAME"/../tests/manifests/python_webhook_pod.yaml
  kubectl --context kind-dranet-test-cluster delete -f "$BATS_TEST_DIRNAME"/../tests/manifests/deviceclass.yaml
  
  docker exec "$NODE_NAME" bash -c "ip link delete dev dummy1 || true"
}
