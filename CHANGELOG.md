# Changelog

All notable changes to this project are documented in this file. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). The fork starts from `networkop/meshnet-cni` at d7c306c (May 2023).

## [Unreleased]

### Changed

- The module is `github.com/emigcm98/meshnet-cni`, built with Go 1.26 and the current Kubernetes client libraries.
- The image is `ghcr.io/emigcm98/meshnet`, published by the Image workflow as `sha-<commit>` on every merge to master and as its tag and `latest` on every `v*` tag.

### Removed

- The Docker Hub release, the kops overlay, the old entrypoint, the proto generation stage of the image and the material of the original README.
