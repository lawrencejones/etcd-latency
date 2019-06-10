# etcd-latency

Required to debug etcd connectivity issues and performance problems. Created
while having issues deploying an etcd cluster behind an L4 load balancer that
poorly handled membership changes.

The most useful command is `pause` in combination with `--grpc-debug-logs`,
enabling a steady stream of etcd health checks along with logging from the grpc
client load balancer. Should be compatible with `ETCDCTL_*` environment
variables.

```
$ etcd-latency --help
usage: etcd-latency [<flags>] <command> [<args> ...]

Flags:
  --help                        Show context-sensitive help (also try --help-long and --help-man).
  --version                     Show application version.
  --grpc-debug-logs             enables grpc debug logging
  --sync-endpoints              attempts to sync endpoints with etcd cluster
  --endpoints="127.0.0.1:2379"  comma separated etcd endpoint list
  --dial-timeout=5s             dial timeout for client connections
  --keepalive-time=2s           keepalive time for client connections
  --keepalive-timeout=6s        keepalive timeout for client connections
  --insecure                    disable transport security for client connections
  --insecure-skip-verify        accept insecure SRV records describing cluster endpoints
  --cert=CERT                   identify secure client using this TLS certificate file
  --key=KEY                     identify secure client using this TLS key file
  --cacert=CACERT               verify certificates of TLS-enabled secure servers using this CA bundle

Commands:
  help [<command>...]
    Show help.

  pause [<flags>]
    pause while connected to cluster running health checks

  move-leader [<transferee>]
    move leader from one etcd member to another

  benchmark [<flags>]
    run various benchmarks against etcd
```
