#!/usr/bin/env bats

load 'test_helper/bats-support/load'
load 'test_helper/bats-assert/load'

setup_file() {
  export BATS_TEST_TIMEOUT=300
  kubectl apply -f "$BATS_TEST_DIRNAME"/../tests/manifests/whereabouts_upstream.yaml
  kubectl -n kube-system wait --for=condition=ready pods -l app=whereabouts --timeout=120s

	# Build and load webhook image
	docker build --load -t dranet/webhook-whereabouts:test -f "$BATS_TEST_DIRNAME"/../cmd/webhook-whereabouts/Dockerfile "$BATS_TEST_DIRNAME"/../
	kind load docker-image dranet/webhook-whereabouts:test --name dranet-test-cluster

	# Deploy whereabouts webhook daemonset and configmap
	kubectl apply -f "$BATS_TEST_DIRNAME"/../tests/manifests/whereabouts_webhook_daemonset.yaml
	kubectl -n kube-system wait --for=condition=ready pods -l app=whereabouts-webhook --timeout=120s

  kubectl patch daemonset dranet -n kube-system --type=json -p='[
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--profile-provider=webhook"},
    {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--webhook-url=http://127.0.0.1:8080"}
  ]'
  kubectl rollout status daemonset/dranet -n kube-system --timeout=120s
  
  # Delete dranet pods to restart them with new config
  kubectl delete pods -n kube-system -l app=dranet
  kubectl wait --for=condition=ready pods --namespace=kube-system -l k8s-app=dranet --timeout=120s
  # Restart dranet pods again to load the new config
  kubectl delete pods -n kube-system -l app=dranet
  kubectl wait --for=condition=ready pods --namespace=kube-system -l k8s-app=dranet --timeout=120s
}

teardown_file() {
  kubectl patch daemonset dranet -n kube-system --type=json -p='[
    {"op": "remove", "path": "/spec/template/spec/containers/0/args/5"},
    {"op": "remove", "path": "/spec/template/spec/containers/0/args/4"}
  ]' || true
  kubectl rollout status daemonset/dranet -n kube-system --timeout=120s
  
  kubectl delete -f "$BATS_TEST_DIRNAME"/../tests/manifests/whereabouts_webhook_daemonset.yaml || true
  
  kubectl delete -f "$BATS_TEST_DIRNAME"/../tests/manifests/whereabouts_upstream.yaml || true
}

@test "validate whereabouts cni integration" {
  for node in $(kind get nodes --name dranet-test-cluster); do
    docker exec "$node" bash -c "ip link add dummy0 type dummy || true"
    docker exec "$node" bash -c "ip link set up dev dummy0 || true"
  done

  kubectl apply -f "$BATS_TEST_DIRNAME"/../tests/manifests/deviceclass.yaml
  kubectl apply -f "$BATS_TEST_DIRNAME"/../tests/manifests/whereabouts-claim.yaml
  kubectl apply -f "$BATS_TEST_DIRNAME"/../tests/manifests/pod-whereabouts.yaml

  kubectl wait --for=condition=ready pod/pod-whereabouts --timeout=120s
  
  # Validate pod has IP from whereabouts range
  run kubectl exec pod-whereabouts -- ip -4 addr show
  assert_output --partial "192.168.100."

  # The webhook API passes the full ResourceClaim, so whereabouts records the
  # claim UID as the allocation identity and the real Pod as podref.
  claim_uid="$(kubectl get resourceclaims -o json | jq -r '.items[] | select(any(.status.reservedFor[]?; .name == "pod-whereabouts")) | .metadata.uid')"
  run kubectl -n kube-system get ippool 192.168.100.0-24 -o json
  assert_success
  allocation_id="$(jq -r '.spec.allocations | to_entries[] | select(.value.podref == "default/pod-whereabouts") | .value.id' <<<"$output")"
  assert_equal "$allocation_id" "$claim_uid"

  kubectl delete pod pod-whereabouts
  kubectl delete resourceclaimtemplate whereabouts-claim
  
  for node in $(kind get nodes --name dranet-test-cluster); do
    docker exec "$node" bash -c "ip link delete dev dummy0 || true"
  done
}
