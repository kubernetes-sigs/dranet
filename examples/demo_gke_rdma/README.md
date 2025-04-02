# GKE RDMA

```
 kubectl apply -f examples/demo_gke_rdma/dranet-gke-accelerator-install.yaml
clusterrole.rbac.authorization.k8s.io/dranet created
clusterrolebinding.rbac.authorization.k8s.io/dranet created
serviceaccount/dranet created
daemonset.apps/dranet created
```

```
kubectl get pods -l k8s-app=dranet -n kube-system
NAME           READY   STATUS    RESTARTS     AGE
dranet-2kvf9   0/1     Running   1 (8s ago)   41s
dranet-56kbh   0/1     Running   1 (8s ago)   41s
dranet-5m9b5   0/1     Running   1 (8s ago)   41s
dranet-65jtv   0/1     Running   1 (8s ago)   41s
dranet-6tkk9   0/1     Running   1 (8s ago)   41s
dranet-6x6r4   0/1     Running   1 (8s ago)   41s
dranet-78d7l   0/1     Running   1 (8s ago)   41s
dranet-7lws2   0/1     Running   1 (8s ago)   41s
dranet-7x27q   0/1     Running   1 (8s ago)   41s
dranet-9chpt   0/1     Running   1 (8s ago)   41s
dranet-9cxvn   0/1     Running   1 (8s ago)   41s
dranet-cg5tt   0/1     Running   1 (8s ago)   41s
dranet-clt6b   0/1     Running   1 (8s ago)   41s
dranet-dfzrs   0/1     Running   1 (8s ago)   41s
dranet-dmpfc   0/1     Running   1 (8s ago)   41s
dranet-f98xg   0/1     Running   1 (8s ago)   41s
dranet-j462k   0/1     Running   1 (8s ago)   41s
dranet-jn8sq   0/1     Running   1 (8s ago)   41s
dranet-kmjkx   0/1     Running   1 (8s ago)   41s
dranet-ktmts   0/1     Running   1 (8s ago)   41s
dranet-l7zfl   0/1     Running   1 (8s ago)   41s
dranet-lt9f4   0/1     Running   1 (8s ago)   41s
dranet-r8g7p   0/1     Running   1 (8s ago)   41s
dranet-rlvlq   0/1     Running   1 (8s ago)   41s
dranet-rntw4   0/1     Running   1 (8s ago)   41s
dranet-sbq6m   0/1     Running   1 (8s ago)   41s
dranet-srh4x   0/1     Running   1 (8s ago)   41s
dranet-tm5cc   0/1     Running   1 (8s ago)   41s
dranet-vwrgt   0/1     Running   1 (8s ago)   41s
dranet-w7mdv   0/1     Running   1 (8s ago)   41s
dranet-x97pt   0/1     Running   1 (8s ago)   41s
dranet-xvhrd   0/1     Running   1 (8s ago)   41s
```


```
kubectl exec nccl-test-host-1 -it -- /usr/local/gib/scripts/run_nccl_tests.sh -t all_gather -b 1K -e 8G nccl-host-1 nccl-host-2
```