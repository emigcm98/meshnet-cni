#!/bin/sh
# Drop the CNI binary on the node, then run the daemon as PID 1 so it gets the kubelet's signals.
set -e
cp /meshnet /opt/cni/bin/meshnet
exec /meshnetd
