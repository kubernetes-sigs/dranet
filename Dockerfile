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

# setup cross-compile env
ARG GOLANG_IMAGE=golang:1.26
ARG BASE_IMAGE=gcr.io/distroless/base-debian12

FROM --platform=$BUILDPLATFORM $GOLANG_IMAGE AS builder
ARG TARGETARCH
# GOFLAGS=-tags=e2e builds the image the end-to-end tests run.
ARG GOARCH=${TARGETARCH} CGO_ENABLED=0 GOFLAGS=""

# cache go modules
WORKDIR /go/src/app
COPY go.mod go.sum .
RUN go mod download

# build
COPY . .
RUN go build -o /go/bin/dranet ./cmd/dranet && \
    go build -o /go/bin/dranet-hook ./cmd/dranet-hook

# copy binary onto base image
FROM $BASE_IMAGE
COPY --from=builder --chown=root:root /go/bin/dranet /dranet
COPY --from=builder --chown=root:root /go/bin/dranet-hook /dranet-hook
CMD ["/dranet"]
