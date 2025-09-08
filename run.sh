#!/usr/bin/env bash
set -euo pipefail

# Must set this before running
: "${MOMENTO_API_KEY:?Set MOMENTO_API_KEY in the environment}"

# Write dnsmasq config
cat >/etc/dnsmasq.conf <<'CONF'
listen-address=127.0.0.1
port=53
bind-interfaces
user=dnsmasq
group=dnsmasq
pid-file=/var/run/dnsmasq.pid

# Name resolution options
resolv-file=/etc/resolv.dnsmasq
cache-size=500
neg-ttl=60
domain-needed
bogus-priv
CONF

echo "nameserver 169.254.169.253" >/etc/resolv.dnsmasq

# Set ephemeral port range and persist
sysctl -w net.ipv4.ip_local_port_range="1024 65535"
echo "net.ipv4.ip_local_port_range = 1024 65535" >/etc/sysctl.d/99-ephemeral-ports.conf
sysctl --system

# Install packages
if command -v yum >/dev/null 2>&1; then
  yum install -y git dnsmasq htop golang
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y git dnsmasq htop golang
elif command -v apt-get >/dev/null 2>&1; then
  apt-get update -y
  apt-get install -y git dnsmasq htop golang
else
  echo "No supported package manager found." >&2
  exit 1
fi

# Enable and start dnsmasq
systemctl enable dnsmasq.service
systemctl restart dnsmasq.service

# Configure systemd-resolved if present
if systemctl list-unit-files | grep -q '^systemd-resolved.service'; then
  echo 'DNS=127.0.0.1' >> /etc/systemd/resolved.conf
  systemctl restart systemd-resolved.service
fi

# Clone and build benchmark
rm -rf /opt/serverless-cache-benchmark
git clone https://github.com/eaddingtonwhite/serverless-cache-benchmark.git /opt/serverless-cache-benchmark
cd /opt/serverless-cache-benchmark
go build -o serverless-cache-benchmark .

# Run benchmark
MOMENTO_API_KEY="$MOMENTO_API_KEY" ./serverless-cache-benchmark run \
  --cache-type momento \
  --momento-cache-name load \
  --momento-create-cache=false \
  --clients 20 \
  --momento-client-worker-count 20 \
  --momento-client-conn-count 1250 \
  --test-time 1800 \
  --data-size 100

