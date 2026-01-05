#!/usr/bin/env bash
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

set -o errexit
set -o nounset
set -o pipefail

# Ensure docker buildx is available
if ! docker buildx version >/dev/null 2>&1; then
  echo "Error: docker buildx is not available. Please install Docker 19.03+ with buildx support."
  exit 1
fi

# Create a builder instance if it doesn't exist
if ! docker buildx inspect dranet-builder >/dev/null 2>&1; then
  echo "Creating buildx builder instance: dranet-builder"
  docker buildx create --name dranet-builder --use
else
  echo "Using existing buildx builder: dranet-builder"
  docker buildx use dranet-builder
fi

docker buildx inspect --bootstrap
