---
title: "Linux Network Namespaces and Interfaces"
date: 2025-06-05T11:20:46Z
---

Network namespaces create isolated network stacks, including network devices, IP addresses, routing tables, rules , ...
This separation is crucial for containerization.

Network namespaces also contain network devices that [can live exactly on one network
namespace](https://man7.org/linux/man-pages/man7/network_namespaces.7.html):

>  physical network device can live in exactly one network
   namespace.  When a network namespace is freed (i.e., when the last
   process in the namespace terminates), its physical network devices
   are moved back to the initial network namespace (not to the
   namespace of the parent of the process).


## Moving a network interface between namespaces

This is achieved using the [`RTM_NEWLINK` netlink message](https://github.com/torvalds/linux/blob/a7f2e10ecd8f18b83951b0bab47ddaf48f93bf47/net/core/rtnetlink.c#L2999-L3023),
along with attributes specifying the target namespace. 

```c
static int do_setlink(const struct sk_buff *skb, struct net_device *dev,
		      struct net *tgt_net, struct ifinfomsg *ifm,
		      struct netlink_ext_ack *extack,
		      struct nlattr **tb, int status)
{
	const struct net_device_ops *ops = dev->netdev_ops;
	char ifname[IFNAMSIZ];
	int err;

	err = validate_linkmsg(dev, tb, extack);
	if (err < 0)
		goto errout;

	if (tb[IFLA_IFNAME])
		nla_strscpy(ifname, tb[IFLA_IFNAME], IFNAMSIZ);
	else
		ifname[0] = '\0';

	if (!net_eq(tgt_net, dev_net(dev))) {
		const char *pat = ifname[0] ? ifname : NULL;
		int new_ifindex;

		new_ifindex = nla_get_s32_default(tb[IFLA_NEW_IFINDEX], 0);

		err = __dev_change_net_namespace(dev, tgt_net, pat, new_ifindex);
```    

These attributes may include:
* `IFLA_NET_NS_PID`: Target namespace identified by process ID.
* `IFLA_NET_NS_FD`: Target namespace identified by file descriptor.
* **`IFLA_IFNAME`**: Specifies the desired name of the interface in the target namespace.


The core function responsible for moving interfaces, `__dev_change_net_namespace`, directly interacts with `IFLA_IFNAME`.
As documented in the kernel API documentation: [\_\_dev\_change\_net\_namespace](https://www.kernel.org/doc/html/latest/networking/kapi.html#c.__dev_change_net_namespace),
this function shuts down a device interface and moves it to a new network namespace.

```c
int __dev_change_net_namespace(struct net_device *dev, struct net *net,
                                   const char *pat, int new_ifindex);
```                                   

Let's see some examples, using `strace` we can see the netlink messages:

We can indicates just the destination namespace, `ip link` gets the interface index by the name and sends a RTM_NEWLINK
message with the IFLA_NET_NS_FD attribute for setting the network namespace:

```sh
# ip netns add ns1
# ip netns exec ns1 ip link
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN mode DEFAULT group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
# ip link add dummy0 type dummy
# strace -e trace=network ip link set dummy0 netns ns1 2>&1 | grep RTM_NEWLINK | grep sendmsg
sendmsg(3, {msg_name={sa_family=AF_NETLINK, nl_pid=0, nl_groups=00000000}, msg_namelen=12, msg_iov=[{iov_base=[{nlmsg_len=40, nlmsg_type=RTM_NEWLINK, nlmsg_flags=NLM_F_REQUEST|NLM_F_ACK, nlmsg_seq=1742478199, nlmsg_pid=0}, {ifi_family=AF_UNSPEC, ifi_type=ARPHRD_NETROM, ifi_index=if_nametoindex("dummy0"), ifi_flags=0, ifi_change=0}, [{nla_len=8, nla_type=IFLA_NET_NS_FD}, 4]], iov_len=40}], msg_iovlen=1, msg_controllen=0, msg_flags=0}, 0) = 40
# ip netns exec ns1 ip link
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN mode DEFAULT group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
24: dummy0: <BROADCAST,NOARP> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/ether 4e:3d:fe:03:2c:c8 brd ff:ff:ff:ff:ff:ff
```

If we try to move a device with a name that already exist in the namespace then we'll have an error
```sh
# ip link set dummy0 netns ns1
RTNETLINK answers: File exists
```

Network interface names have a long history of issues, more on https://wiki.debian.org/NetworkInterfaceNames.

And since names MUST be unique per network namespace, we can do a "change network namespace and rename" operation at the same time.
In this case, we are going to rename `dummy0` to `othername`, and see how we have the `IFLA_IFNAME` attribute now.

```sh
~# strace -e trace=network ip link set dummy0 netns ns1 name othername 2>&1 | grep RTM_NEWLINK | grep sendmsg
sendmsg(3, {msg_name={sa_family=AF_NETLINK, nl_pid=0, nl_groups=00000000}, msg_namelen=12, msg_iov=[{iov_base=[{nlmsg_len=56, nlmsg_type=RTM_NEWLINK, nlmsg_flags=NLM_F_REQUEST|NLM_F_ACK, nlmsg_seq=1742479211, nlmsg_pid=0}, {ifi_family=AF_UNSPEC, ifi_type=ARPHRD_NETROM, ifi_index=if_nametoindex("dummy0"), ifi_flags=0, ifi_change=0}, [[{nla_len=8, nla_type=IFLA_NET_NS_FD}, 4], [{nla_len=14, nla_type=IFLA_IFNAME}, "othername"]]], iov_len=56}], msg_iovlen=1, msg_controllen=0, msg_flags=0}, 0) = 56
jkenj1:~# ip netns exec ns1 ip link
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN mode DEFAULT group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
24: dummy0: <BROADCAST,NOARP> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/ether 4e:3d:fe:03:2c:c8 brd ff:ff:ff:ff:ff:ff
27: othername: <BROADCAST,NOARP> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/ether b6:a9:c0:0f:41:39 brd ff:ff:ff:ff:ff:ff
```

However, there is also another nice property if we don't mind about the destination name and just the prefix,
and is that we can use template by appending `%d` to the IFLA_IFNAME attribute, and the interface name will be add an
unique suffix to avoid to break:

```sh
# ip link add dummy0 type dummy
# ip link set dummy0 netns ns1 name othername
RTNETLINK answers: File exists
# ip link set dummy0 netns ns1 name othername%d
jkenj1:~# ip netns exec ns1 ip link
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN mode DEFAULT group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
24: dummy0: <BROADCAST,NOARP> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/ether 4e:3d:fe:03:2c:c8 brd ff:ff:ff:ff:ff:ff
27: othername: <BROADCAST,NOARP> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/ether b6:a9:c0:0f:41:39 brd ff:ff:ff:ff:ff:ff
28: othername1: <BROADCAST,NOARP> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/ether 16:96:7c:ab:33:bb brd ff:ff:ff:ff:ff:ff
```

## Namespace Deletion and Cleanup

We saw how to move interfaces between network namespaces, that applies in both direction, I can move the interface
from the root namespace to the network namespace and viceversa, but what happens when the network namespace is deleted.

Checking at https://github.com/torvalds/linux/blob/a7f2e10ecd8f18b83951b0bab47ddaf48f93bf47/net/core/dev.c#L12370

```c
static void __net_exit default_device_exit_net(struct net *net)
{
	struct netdev_name_node *name_node, *tmp;
	struct net_device *dev, *aux;
	/*
	 * Push all migratable network devices back to the
	 * initial network namespace
	 */
	ASSERT_RTNL();
	for_each_netdev_safe(net, dev, aux) {
		int err;
		char fb_name[IFNAMSIZ];

		/* Ignore unmoveable devices (i.e. loopback) */
		if (dev->netns_local)
			continue;

		/* Leave virtual devices for the generic cleanup */
		if (dev->rtnl_link_ops && !dev->rtnl_link_ops->netns_refund)
			continue;

		/* Push remaining network devices to init_net */
		snprintf(fb_name, IFNAMSIZ, "dev%d", dev->ifindex);
		if (netdev_name_in_use(&init_net, fb_name))
			snprintf(fb_name, IFNAMSIZ, "dev%%d");

		netdev_for_each_altname_safe(dev, name_node, tmp)
			if (netdev_name_in_use(&init_net, name_node->name))
				__netdev_name_node_alt_destroy(name_node);

		err = dev_change_net_namespace(dev, &init_net, fb_name);
		if (err) {
			pr_emerg("%s: failed to move %s to init_net: %d\n",
				 __func__, dev->name, err);
			BUG();
		}
	}
}
```

we can see that the logic is as follow:
- unmoveable device and virtual devices will not be moved back
- remaining network devices (physical interfaces, ...) will be moved back:
  -  with the existing name if possible, or with the `dev%d` or `dev%%d` template name

If you wonder about virtual vs physical, you can just check the sysfs filesystem

```sh
~# ls -al /sys/class/net/
total 0
drwxr-xr-x  2 root root    0 Mar 20 07:26 .
drwxr-xr-x 71 root root    0 Mar 20 07:26 ..
lrwxrwxrwx  1 root root    0 Mar 20 07:26 adp0 -> ../../devices/virtual/net/adp0
lrwxrwxrwx  1 root root    0 Mar 20 07:26 adp1 -> ../../devices/virtual/net/adp1
lrwxrwxrwx  1 root root    0 Mar 20 07:26 adp2 -> ../../devices/virtual/net/adp2
lrwxrwxrwx  1 root root    0 Mar 20 07:26 adp3 -> ../../devices/virtual/net/adp3
-rw-r--r--  1 root root 4096 Mar 20 07:26 bonding_masters
lrwxrwxrwx  1 root root    0 Mar 20 07:26 dcn1 -> ../../devices/pci0000:57/0000:57:00.0/0000:58:00.0/net/dcn1
lrwxrwxrwx  1 root root    0 Mar 20 07:26 dcn2 -> ../../devices/pci0000:97/0000:97:00.0/0000:98:00.0/net/dcn2
lrwxrwxrwx  1 root root    0 Mar 20 07:26 dev0 -> ../../devices/pci0000:d7/0000:d7:00.0/0000:d8:00.0/net/dev0
lrwxrwxrwx  1 root root    0 Mar 20 07:26 dev7 -> ../../devices/virtual/net/dev7
lrwxrwxrwx  1 root root    0 Mar 20 07:26 eth0 -> ../../devices/virtual/net/eth0
lrwxrwxrwx  1 root root    0 Mar 20 07:26 eth1 -> ../../devices/pci0000:37/0000:37:00.0/0000:38:00.0/net/eth1
```

As you can see in the list above, I just moved one physical interface to a network namespace and created a dummy device with the same name with the same name, once I deleted the namespace with the physical interface it came back to the root namespcae as `dev0`

## Alternative Names

Linux [added in 2019 the capability to set alternative names on network interfaces](https://patchwork.ozlabs.org/project/netdev/cover/20190930094820.11281-1-jiri@resnulli.us/#2269624), mainly to avoid the
current length limitation

```sh
 ip link property add dev DEVICE [ altname NAME .. ]
 ip link property del dev DEVICE [ altname NAME .. ]
```

However, this will not help to solve the name conflict problem, just the opposite, since it increases the risk of collision
```
# ip netns exec ns1 ip link
1: lo: <LOOPBACK> mtu 65536 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
2: dummy0: <BROADCAST,NOARP> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/ether 4a:02:e1:87:96:07 brd ff:ff:ff:ff:ff:ff
# ip link property add dev dummy1 altname dummy0
# ip a
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host
       valid_lft forever preferred_lft forever
5: dummy1: <BROADCAST,NOARP> mtu 1500 qdisc noop state DOWN group default qlen 1000
    link/ether 6e:08:0a:b4:15:11 brd ff:ff:ff:ff:ff:ff
    altname dummy0
# ip link set dummy0 netns ns1
RTNETLINK answers: File exists
```

## Interfaces naming with systemd

Linux network interface naming has evolved to provide more predictable and consistent names, moving away from the older, less reliable "ethX" scheme. Systemd's systemd.net-naming-scheme plays a crucial role in this.

The core idea is to assign names based on hardware and firmware information, making them independent of the order in which devices are detected. This prevents interface names from changing unexpectedly, especially after hardware changes or kernel updates. The scheme prioritizes different naming types, falling back to less specific methods when necessary.

systemd-udevd assigns predictable network interface names based on hardware info (firmware, PCI, MAC) via rules in /etc/udev/rules.d/. It listens for kernel uevents and names interfaces like ens1 or wlp2s0. systemd-networkd then uses these consistent names to apply network configurations from /etc/systemd/network/, ensuring stable network settings across reboots. 

We can use `udevadm monitor` to monitor:
- UDEV: the event which udev sends out after rule processing
- KERNEL: the kernel uevent

### Virtual interfaces

When we create a virtual interface
```sh
# ip link add dummy3 type dummy
```
we get following events:
```
KERNEL[160479.602268] add      /devices/virtual/net/dummy3 (net)
KERNEL[160479.602296] add      /devices/virtual/net/dummy3/queues/rx-0 (queues)
KERNEL[160479.602305] add      /devices/virtual/net/dummy3/queues/tx-0 (queues)
UDEV  [160479.602891] add      /devices/virtual/net/dummy3 (net)
UDEV  [160479.603089] add      /devices/virtual/net/dummy3/queues/rx-0 (queues)
UDEV  [160479.603254] add      /devices/virtual/net/dummy3/queues/tx-0 (queues)
```

If we move the virtual interface to a namespace:
```sh
# ip netns add ns1
# ip link set dummy3 netns ns1
```

we get

```
KERNEL[160552.155591] remove   /devices/virtual/net/dummy3 (net)
UDEV  [160552.156038] remove   /devices/virtual/net/dummy3 (net)
```

If we move it back:
```sh
#ip netns exec ns1 ip link set dummy3 netns  root
```
we get
```
KERNEL[160597.604155] add      /devices/virtual/net/dummy3 (net)
KERNEL[160597.604200] move     /devices/virtual/net/dummy3 (net)
UDEV  [160597.605054] add      /devices/virtual/net/dummy3 (net)
UDEV  [160597.605453] move     /devices/virtual/net/dummy3 (net)
```

However, since it is a virtual interface, when we delete the namespace it just disappears.

### Physical interfaces

Physical interfaces are similar, the only difference is that when the namespace is deleted,
they come back to the root namespace ... and if the name collide, they are renamed.

In this example I move the `dev0` interface to a namespace.
Then create a virtual interface `dev0` in the root namespace.
And then delete the namespace.

```sh
# ip netns add ns1
# ip link set dev0 netns ns1
# ip link add dev0 type dummy
# ip netns del ns1
```

```
KERNEL[159662.875424] remove   /devices/pci0000:d7/0000:d7:00.0/0000:d8:00.0/net/dev0 (net)
UDEV  [159662.875849] remove   /devices/pci0000:d7/0000:d7:00.0/0000:d8:00.0/net/dev0 (net)


KERNEL[159679.855918] add      /devices/virtual/net/dev0 (net)
KERNEL[159679.855938] add      /devices/virtual/net/dev0/queues/rx-0 (queues)
KERNEL[159679.855946] add      /devices/virtual/net/dev0/queues/tx-0 (queues)
UDEV  [159679.856473] add      /devices/virtual/net/dev0 (net)
UDEV  [159679.856673] add      /devices/virtual/net/dev0/queues/rx-0 (queues)
UDEV  [159679.856844] add      /devices/virtual/net/dev0/queues/tx-0 (queues)

KERNEL[159695.029543] add      /devices/pci0000:d7/0000:d7:00.0/0000:d8:00.0/net/dev0 (net)
KERNEL[159695.029565] move     /devices/pci0000:d7/0000:d7:00.0/0000:d8:00.0/net/dev1 (net)
UDEV  [159695.030036] add      /devices/pci0000:d7/0000:d7:00.0/0000:d8:00.0/net/dev0 (net)
UDEV  [159695.030665] move     /devices/pci0000:d7/0000:d7:00.0/0000:d8:00.0/net/dev1 (net)
```
