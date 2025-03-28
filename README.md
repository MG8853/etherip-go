# etherip-go
Go言語でEtherIP(RFC3378)を動かすなにかです。

## Sample
```bash
sudo ./etherip -config config.yaml
```

Example config.yaml
```yaml
# IP version (4 or 6)
version: 6

# Tap Number (example: tap0)
tap_name: tap127

# MTU (example: 1500)
mtu: 1500

# Auto Get Interface IP Address (example: ens18)
src_iface: eth0

# Dst Address (FQDN or IP) 
dst_host: 2001::1

# FQDN Resolve Interval
resolve_interval: 10m
```



