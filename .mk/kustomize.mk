.PHONY: kust-install
# kustomize 5.8 asks for a newer Go than the module's: let go fetch that toolchain even where GOTOOLCHAIN is local, as setup-go sets it.
kust-install:
	GOTOOLCHAIN=auto go install sigs.k8s.io/kustomize/kustomize/v5@v5.8.3

.PHONY: kust-ensure
kust-ensure:
	@which $(GOPATH)/bin/kustomize >/dev/null 2>&1 || \
		make kust-install

.PHONY: kustomize
kustomize: kust-ensure
	cd manifests/overlays/e2e && $(GOPATH)/bin/kustomize edit set image ${DOCKER_IMAGE}:${COMMIT}

