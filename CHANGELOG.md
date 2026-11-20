# Changelog

All notable changes to this project are documented in this file. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). The fork starts from `networkop/meshnet-cni` at d7c306c (May 2023).

## [Unreleased]

### Changed

- The module is `github.com/emigcm98/meshnet-cni`, built with Go 1.26 and the current Kubernetes client libraries.
- The image is `ghcr.io/emigcm98/meshnet`, published by the Image workflow as `sha-<commit>` on every merge to master and as its tag and `latest` on every `v*` tag.
- The CI checks gofmt and vet before the unit tests, and runs the three-node e2e over VXLAN and over gRPC wires on a kind of two workers, with kind 0.33 and kustomize 5.8.
- `tests/eviction.sh` and `make eviction`: a pod with a cross-worker link deleted and recreated on its peer's worker and back, six steps with a ping each; the vxlan-to-veth steps fail on this line until the daemon reconciles links.

### Fixed

- `make install` and `make uninstall` use the kustomize the Makefile installs instead of one expected on the PATH.
- `meshnetd` waits for the primary CNI to leave its configuration instead of exiting when it starts first, stops on SIGTERM so its cleanup runs, and the DaemonSet is Ready only once meshnet is chained into the node's CNI configuration.

### Removed

- The Docker Hub release, the kops overlay, the old entrypoint, the proto generation stage of the image and the material of the original README.
- The gRPC wires (`daemon/grpcwire`, their CRD, overlays, RPCs and docs, and `INTER_NODE_LINK_TYPE`): a userspace datapath measured at 20 Mbit/s and 13 ms where VXLAN gives 10 Gbit/s and 0.23 ms. Without libpcap `meshnetd` is a static binary and the image runs on busybox.
