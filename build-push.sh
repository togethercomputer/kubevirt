#!/usr/bin/env bash

set -euo pipefail

# this script depends on being logged in to the "together.ai - prod" public
# ECR registry; to login run
# aws ecr-public get-login-password --region us-east-1 | docker login --username AWS --password-stdin public.ecr.aws

export DOCKER_PREFIX='public.ecr.aws/k6t4m3l7/kubevirt'
DOCKER_TAG="$(git describe --tags)"
export DOCKER_TAG
PUSH_TARGETS="${PUSH_TARGETS:-"virt-controller"}"

read -rp "Build tag $DOCKER_TAG for ${PUSH_TARGETS// /, } image(s)? [y/N] " response

if [ "$response" = 'y' ] || [ "$response" = 'Y' ]; then
  make bazel-push-images PUSH_TARGETS="$PUSH_TARGETS"
fi
