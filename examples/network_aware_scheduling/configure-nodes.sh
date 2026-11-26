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

# Give each fabric interface of each node its address and its own routing
# table, as a host on a fabric with more than one rail has: traffic from the
# address of a rail leaves through the interface of this rail. DRANET moves
# the addresses, the routes and the rule to the Pod that gets the interface.
#
# Leaf N is the subnet 198.18.N.0/24 with the gateway 198.18.N.1.
#
# DRANET does not put the addresses back when an interface returns from a
# Pod. Run this script again after the Pods of a step are gone. It does not
# configure an interface that is not in its node at this time, for example
# because a Pod has it, and prints a message for it.

set -eu

configure() {
  local node="$1" ifname="$2" address="$3"
  local subnet="${address%.*}"
  local table=$((100 + ${subnet##*.}))
  docker exec "$node" sh -c "
    if ! ip link show dev $ifname >/dev/null 2>&1; then
      echo '$node: $ifname is not in the node, not configured' >&2
      exit 0
    fi
    set -e
    ip link set dev $ifname up
    ip addr replace $address/24 dev $ifname
    ip route replace $subnet.0/24 dev $ifname table $table
    ip route replace 198.18.0.0/16 via $subnet.1 dev $ifname table $table
    while ip rule del from $address lookup $table 2>/dev/null; do :; done
    ip rule add from $address lookup $table
  "
}

#         node         interface  address
configure tas-worker   eth1       198.18.1.12
configure tas-worker   eth2       198.18.2.12
configure tas-worker2  eth1       198.18.1.13
configure tas-worker2  eth2       198.18.2.13
configure tas-worker3  eth1       198.18.3.12
configure tas-worker3  eth2       198.18.4.12
