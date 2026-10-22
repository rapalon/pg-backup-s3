#!/bin/sh
# One-shot initialisation of the single-node Garage in the integration stand:
# wait for the node, assign a layout, create the bucket and import a fixed key.
# Idempotent: safe to run again against an already initialised node.
set -eu

BUCKET="${GARAGE_BUCKET:-pgbackup-test}"
KEY_ID="${GARAGE_KEY_ID:?GARAGE_KEY_ID is required}"
KEY_SECRET="${GARAGE_KEY_SECRET:?GARAGE_KEY_SECRET is required}"

g() { garage -c /etc/garage.toml "$@"; }

echo "garage-init: waiting for the node"
i=0
until g status >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then echo "garage-init: node did not come up" >&2; exit 1; fi
  sleep 1
done

node="$(g node id -q | cut -d@ -f1)"
if g layout show 2>/dev/null | grep -q "^==== CURRENT CLUSTER LAYOUT ====" && g layout show | grep -q "$(echo "$node" | cut -c1-16)"; then
  echo "garage-init: layout already applied"
else
  echo "garage-init: assigning layout to node $node"
  g layout assign -z dc1 -c 1G "$node"
  g layout apply --version 1
fi

if g bucket info "$BUCKET" >/dev/null 2>&1; then
  echo "garage-init: bucket $BUCKET exists"
else
  g bucket create "$BUCKET"
fi

if g key info "$KEY_ID" >/dev/null 2>&1; then
  echo "garage-init: key already imported"
else
  g key import --yes -n pgbackup-test "$KEY_ID" "$KEY_SECRET"
fi

g bucket allow --read --write --owner "$BUCKET" --key "$KEY_ID"
g bucket info "$BUCKET"
echo "garage-init: done"
