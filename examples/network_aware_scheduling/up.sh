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

# Start the lab of the example and install DRANET in it.
#
# Environment:
#   DRANET_IMAGE  A DRANET image to use. Default: build one from this
#                 repository.
#   LAB_DIR       The directory for the files of the lab. Default:
#                 _artifacts/network-aware-scheduling in this repository.

set -eu

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
LAB_DIR="${LAB_DIR:-$REPO/_artifacts/network-aware-scheduling}"
IMAGE="${DRANET_IMAGE:-}"

# DRANET does not publish veth interfaces by default, and the interfaces of
# the fabric are veth interfaces in this lab. Publish only these.
FILTER='"dra.net/ifName" in attributes \&\& attributes["dra.net/ifName"].StringValue.matches("^eth[1-9]")'

for tool in netlab containerlab kind kubectl docker; do
  command -v "$tool" >/dev/null || { echo "$tool is not installed" >&2; exit 1; }
done

if [[ -z "$IMAGE" ]]; then
  IMAGE="registry.k8s.io/networking/dranet:example"
  # --load: a builder with the docker-container driver does not put the
  # image in the local image store without it.
  docker build -t "$IMAGE" -f "$REPO/Dockerfile" "$REPO" --load
fi

rm -rf "$LAB_DIR"
(
  # containerlab makes the files of a switch with the permissions of the lab
  # directory, and SR Linux must be able to read them.
  umask 022
  mkdir -p "$LAB_DIR"
  cp -r "$HERE"/topology.yml "$HERE"/kind.yaml "$HERE"/configs "$LAB_DIR"/
  cd "$LAB_DIR"
  # containerlab creates the kind cluster, and kind adds it to the
  # kubeconfig. Give it a file of the lab, so that it does not change the
  # kubeconfig of the user. containerlab makes this file as root.
  KUBECONFIG="$LAB_DIR/kubeconfig.containerlab" netlab up --no-config topology.yml
)
export KUBECONFIG="$LAB_DIR/kubeconfig"
(
  umask 077
  kind get kubeconfig --name tas > "$KUBECONFIG"
)

kind load docker-image "$IMAGE" --name tas
for node in $(kind get nodes --name tas); do
  docker exec "$node" mount -t bpf bpffs /sys/fs/bpf
  docker exec "$node" mount --make-shared /sys/fs/bpf
done

"$HERE"/configure-nodes.sh

sed -e "s#registry.k8s.io/networking/dranet.*#$IMAGE#" \
  -e "s|--v=4|--v=4\n        - '--filter=$FILTER'|" \
  < "$REPO/install.yaml" | kubectl apply -f -

# kube-proxy fails on a host that has no free inotify instances
# (fs.inotify.max_user_instances). DRANET on a worker cannot reach the API
# server then.
kubectl rollout status daemonset/kube-proxy --namespace=kube-system --timeout=120s
kubectl rollout status daemonset/dranet --namespace=kube-system --timeout=180s

echo "Waiting for DRANET to publish the interfaces of the fabric"
for _ in $(seq 60); do
  devices="$(kubectl get resourceslices -o jsonpath='{range .items[*]}{range .spec.devices[*]}{.name}{"\n"}{end}{end}' | grep -c '^eth[12]$' || true)"
  [[ "$devices" == 6 ]] && break
  sleep 1
done
if [[ "$devices" != 6 ]]; then
  echo "DRANET published $devices of the 6 interfaces of the fabric" >&2
  exit 1
fi

echo
echo "The lab runs. Use the cluster with:"
echo "  export KUBECONFIG=$KUBECONFIG"
