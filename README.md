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

# Tap Number (tap0)
## 変更してもtap0を作成できないとエラー発生するよ
tap_name: tap127

# Auto Bridge (br0 or off)
br_name: br0

# MTU (1500)
mtu: 1500

# Auto Get Interface IP Address (ens18)
src_iface: eth0

# Dst Address (FQDN or IP) 
dst_host: ???

# FQDN Resolve Interval (10s, 1m)
resolve_interval: 10s
```



