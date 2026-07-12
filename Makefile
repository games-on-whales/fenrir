
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