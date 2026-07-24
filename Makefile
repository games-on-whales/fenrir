
.PHONY: help
## Print this help message (based on hslib's answer https://stackoverflow.com/questions/35730218/how-to-automatically-generate-a-makefile-help-command)
help:
	@awk '/^## / \
		{ if (c) {print c}; c=substr($$0, 4); next } \
		 c && /(^[[:alpha:]][[:alnum:]_-]+:)/ \
		{print $$1, "\t", c; c=0} \
		 END { print c }' $(MAKEFILE_LIST)
## Run golangcli lint for code recommendations
lint:
	@golangci-lint run
## Run golangcli fmt to format the code
fmt:
	@golangci-lint fmt
## Run golangcli fmt --diff to see the difference
fmt-diff:
	@golangci-lint fmt --diff
## Run go test
test:
	@go test -race -shuffle=on -timeout 5m ./...

## Run go vet
vet:
	@go vet ./...

## Run the hack script update-codegen.sh
## Which updates the generated codebase
codegen:
	/bin/bash -c $(CURDIR)/hack/update-codegen.sh

## Creates the local kind-cluster used for development
## This cluster will be called "direwolf-cluster" in kind
## and kind-direwolf-cluster in kube contexts
cluster-create:
	@kind create cluster --config=$(CURDIR)/hack/kind-cluster-config.yaml

## deletes the local kind-cluster created by hack/kind-cluster-config.yaml
## I need to find a better way to get the cluster name than to assume it's the same as 
## the file that creates it
cluster-delete:
	@kind delete cluster --name direwolf-cluster

## This sets up the resources needed for the cluster to operator
## resources such as cert-manager
cluster-setup:
	@kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.0/cert-manager.yaml --context=kind-direwolf-cluster