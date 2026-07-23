.PHONY: build test test-unit test-integration test-e2e e2e-vendor test-webhook fuzz-smoketest lint fmt vet tidy coverage docker-build docker-push deploy-dev deploy-prod verify verify-structure verify-manifests verify-docker verify-dirty clean envtest

IMG ?= flux-drift-webhook:latest

# Per-target duration for the native Go fuzz smoke test.
FUZZ_TIME ?= 20s

## envtest tooling for the integration suite.
# The setup-envtest TOOL is fetched via GOPROXY; the kube-apiserver/etcd ASSET
# archives are NOT served by GOPROXY — setup-envtest downloads them from an
# external GitHub index (raw.githubusercontent.com), which works over the
# network on both CI runners and workstations.
# Pinned (not @latest): test-integration is a CI gate and must not depend on a
# moving tool version. Fall back to 1.35.0 if 1.36.0 assets are unavailable;
# in restricted networks, pre-seed $(LOCALBIN) and run with
# ENVTEST_INSTALLED_ONLY=1, or point --index at a mirror.
# Third-party manifests vendored under e2e/ so `make test-e2e` runs offline
# straight after a clone. Refresh them with `make e2e-vendor`.
CERT_MANAGER_VERSION ?= v1.21.0
PROMETHEUS_OPERATOR_VERSION ?= v0.92.1

LOCALBIN ?= $(CURDIR)/bin
ENVTEST ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.24.1
ENVTEST_K8S_VERSION ?= 1.36.0

## Build
build:
	go build -o bin/webhook ./cmd/webhook

## Test
test:
	CGO_ENABLED=0 go test -v -coverprofile=coverage.out ./...

test-race:
	CGO_ENABLED=1 go test -v -race -coverprofile=coverage.out ./...

test-unit:
	CGO_ENABLED=0 go test -v -short ./...

## Smoke-test every native Go fuzz target (discovered across ./internal) by
## running each for FUZZ_TIME; ensures the fuzzers build and find no shallow
## crash. Seed corpora also run as part of `make test`.
fuzz-smoketest:
	@set -e; for file in $$(grep -rl --include='*_test.go' 'func Fuzz' internal/); do \
		dir=$$(dirname "$$file"); \
		for target in $$(grep -oE 'func (Fuzz[A-Za-z0-9_]+)' "$$file" | sed 's/func //'); do \
			echo "==> fuzzing $$target in ./$$dir for $(FUZZ_TIME)"; \
			go test -run='^$$' -fuzz="^$$target$$" -fuzztime=$(FUZZ_TIME) "./$$dir"; \
		done; \
	done

## Install the envtest binaries into $(LOCALBIN) and print the asset directory.
envtest:
	$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path

test-integration: envtest
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test -v -tags=integration ./...

test-e2e:
	./e2e/run-e2e.sh

## Re-download the vendored e2e manifests at the pinned versions above.
## They are committed, so this is only needed when bumping a version.
e2e-vendor:
	curl -sSLf -o e2e/cert-manager.yaml \
		"https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml"
	curl -sSLf -o e2e/podmonitor-crd.yaml \
		"https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/$(PROMETHEUS_OPERATOR_VERSION)/example/prometheus-operator-crd/monitoring.coreos.com_podmonitors.yaml"
	@echo "Vendored cert-manager $(CERT_MANAGER_VERSION) and prometheus-operator $(PROMETHEUS_OPERATOR_VERSION)"

test-webhook:
	./e2e/test-webhook.sh

## Coverage
coverage: test
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

## Lint & format
lint:
	golangci-lint run ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy -compat=1.26

## Docker
docker-build:
	docker build -t $(IMG) .

docker-push:
	docker push $(IMG)

## Deploy
deploy-dev:
	kustomize build deploy/overlays/dev | kubectl apply -f -

deploy-prod:
	kustomize build deploy/overlays/prod | kubectl apply -f -

undeploy:
	kustomize build deploy/base | kubectl delete -f -

## Clean
clean:
	rm -rf bin/ $(LOCALBIN) coverage.out coverage.html coverage.xml

## Generate
generate:
	go generate ./...

## Verification targets

verify-structure:
	@echo "Verifying project structure..."
	@test -f go.mod || (echo "ERROR: go.mod not found" && exit 1)
	@test -f go.sum || (echo "ERROR: go.sum not found" && exit 1)
	@test -d cmd/webhook || (echo "ERROR: cmd/webhook not found" && exit 1)
	@test -d internal/config || (echo "ERROR: internal/config not found" && exit 1)
	@test -d internal/webhook || (echo "ERROR: internal/webhook not found" && exit 1)
	@test -d internal/metrics || (echo "ERROR: internal/metrics not found" && exit 1)
	@test -d internal/controller || (echo "ERROR: internal/controller not found" && exit 1)
	@test -d deploy/base || (echo "ERROR: deploy/base not found" && exit 1)
	@test -d deploy/overlays/dev || (echo "ERROR: deploy/overlays/dev not found" && exit 1)
	@test -d deploy/overlays/prod || (echo "ERROR: deploy/overlays/prod not found" && exit 1)
	@echo "Project structure OK"

verify-build:
	@echo "Verifying Go compilation..."
	go build -o /dev/null ./...
	@echo "Go compilation OK"

verify-manifests:
	@echo "Verifying Kustomize manifests..."
	kustomize build deploy/base > /dev/null
	kustomize build deploy/overlays/dev > /dev/null
	kustomize build deploy/overlays/prod > /dev/null
	@echo "Validating with kubectl dry-run..."
	kustomize build deploy/base | kubectl apply --dry-run=client -f - > /dev/null
	kustomize build deploy/overlays/dev | kubectl apply --dry-run=client -f - > /dev/null
	kustomize build deploy/overlays/prod | kubectl apply --dry-run=client -f - > /dev/null
	@echo "Kustomize manifests OK"

verify-docker:
	@echo "Verifying Docker build..."
	docker build -t $(IMG)-test .
	docker run --rm $(IMG)-test --help > /dev/null 2>&1 || true
	docker rmi $(IMG)-test > /dev/null 2>&1 || true
	@echo "Docker build OK"

verify-dirty:
	@echo "Checking the working tree is clean after fmt/vet/tidy/generate..."
	@if ! git diff --quiet; then \
		echo "ERROR: working tree is dirty; run 'make fmt vet tidy generate' and commit the result:"; \
		git --no-pager diff --stat; \
		exit 1; \
	fi
	@echo "Working tree clean"

verify: fmt vet tidy generate lint verify-structure verify-build test verify-manifests verify-dirty
	@echo "All verifications passed!"

ci: verify verify-docker test-integration fuzz-smoketest
	@echo "CI pipeline completed successfully!"
