/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package slaac implements what the driver needs of IPv6 stateless address
// autoconfiguration (RFC 4862) to configure a Pod interface the way it
// configures one from a DHCP lease: it sends Router Solicitations on the host
// interface, receives the Router Advertisements of the link (RFC 4861), and
// derives from one the addresses and routes the Pod is configured with.
//
// Messages travel over a packet socket rather than the kernel's IPv6 stack, so
// the exchange works on an interface that has no IPv6 address yet, or whose
// IPv6 the kernel has disabled, the same way a DHCPv4 client works on an
// interface without an IPv4 address. What the caller configures from the
// result is up to the caller; the kernel's own IPv6 stack, where it is enabled
// on the interface, still processes the advertisements it receives as usual.
//
// The package is internal: it implements only the part of Neighbor Discovery
// the driver uses, plus the router's side of it that its tests use to answer
// solicitations, and is not meant as a general-purpose library.
package slaac
