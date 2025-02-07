# !/bin/bash
export DOCKER_PREFIX=public.ecr.aws/k6t4m3l7/kubevirt
export DOCKER_TAG=eng-12866

make && make push 