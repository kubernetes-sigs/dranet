#!/bin/bash

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

# Show where two Pods of a job run and the path between their interfaces on
# each rail.
#
# Usage: show-paths.sh <pod> <pod>
#
# The TTL of the ping reply shows the path. 64: the two interfaces are on one
# leaf, with no router between them. 61: the traffic goes through a leaf, the
# spine and a leaf.

set -eu

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <pod> <pod>" >&2
  exit 1
fi

address() {
  kubectl exec "$1" -- ip -4 -o addr show dev "$2" | awk '{print $4}' | cut -d/ -f1
}

for pod in "$1" "$2"; do
  node="$(kubectl get pod "$pod" -o jsonpath='{.spec.nodeName}')"
  group="$(kubectl get node "$node" -o jsonpath='{.metadata.labels.fabric\.example\.com/leaf-group}')"
  echo "$pod: node $node, leaf group $group"
done

for rail in rail1 rail2; do
  from="$(address "$1" "$rail")"
  to="$(address "$2" "$rail")"
  ttl="$(kubectl exec "$1" -- ping -c 2 -W 2 -I "$from" "$to" | sed -n 's/.*ttl=\([0-9]*\).*/\1/p' | tail -1)"
  echo "$rail: $from -> $to: ttl=${ttl:-no reply}"
done
