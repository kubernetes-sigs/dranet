# Copyright The Kubernetes Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

REPO_ROOT:=${CURDIR}
OUT_DIR=$(REPO_ROOT)/bin

# disable CGO by default for static binaries
CGO_ENABLED=0
export GOROOT GO111MODULE CGO_ENABLED

build: build-dranet build-dranetctl

build-dranet:
	go build -v -o "$(OUT_DIR)/dranet" ./cmd/dranet

build-dranetctl:
	go build -v -o "$(OUT_DIR)/dranetctl" ./cmd/dranetctl

clean:
	rm -rf "$(OUT_DIR)/"

test:
	CGO_ENABLED=1 go test -v -race -count 1 ./...

e2e-test:
	bats --verbose-run tests/

# code linters
lint:
	hack/lint.sh

helm-lint:
	helm lint --strict deployments/helm/dranet

update:
	go mod tidy

.PHONY: ensure-buildx
ensure-buildx:
	./hack/init-buildx.sh

HELM_VERSION_SHA?=a2369ca71c0ef633bf6e4fccd66d634eb379b371 # v3.20.1
.PHONY: ensure-helm
ensure-helm:
	@if ! helm version >/dev/null 2>&1; then \
		echo "Helm not found, installing helm@$(HELM_VERSION_SHA) ..."; \
		go install helm.sh/helm/v3/cmd/helm@$(HELM_VERSION_SHA); \
	fi

IMAGE_NAME=dranet
REGISTRY?=gcr.io/k8s-staging-networking
TAG?=$(shell git describe --tags --always --dirty)
CHART_VERSION?=$(shell echo "$(TAG)" | sed 's/^v//')

IMAGE?=$(REGISTRY)/$(IMAGE_NAME):$(TAG)

CHART_REGISTRY?=$(REGISTRY)/charts
PLATFORMS?=linux/amd64,linux/arm64

# base images (defaults are in the Dockerfile)
BUILD_ARGS?=
ifdef GOLANG_IMAGE
BUILD_ARGS+=--build-arg GOLANG_IMAGE=$(GOLANG_IMAGE)
endif
ifdef BASE_IMAGE
BUILD_ARGS+=--build-arg BASE_IMAGE=$(BASE_IMAGE)
endif

# required to enable buildx
export DOCKER_CLI_EXPERIMENTAL=enabled
image-build: ensure-buildx
	docker buildx build . \
		$(BUILD_ARGS) \
		--tag="${IMAGE}" \
		--load

image-push: ensure-buildx
	docker buildx build . \
		--platform=$(PLATFORMS) \
		$(BUILD_ARGS) \
		--tag="${IMAGE}" \
		--push

helm-package: ensure-helm
	helm package deployments/helm/dranet \
		--version "$(CHART_VERSION)" \
		--app-version "$(TAG)" \
		--destination $(OUT_DIR)

helm-push: helm-package
	helm push $(OUT_DIR)/dranet-$(CHART_VERSION).tgz oci://$(CHART_REGISTRY)

kind-cluster:
	kind create cluster --name dra --config kind.yaml

kind-image: image-build
	docker tag ${IMAGE} registry.k8s.io/networking/dranet:stable
	kind load docker-image registry.k8s.io/networking/dranet:stable --name dra
	kubectl delete -f install.yaml || true
	kubectl apply -f install.yaml

# The main release target, which pushes all images and helm charts.
release: image-push helm-push


