.PONY: all build deps image lint test

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {sub("\\\\n",sprintf("\n%22c"," "), $$2);printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

all: test build ## Run the tests and build the binary.

build: ## Build the binary.
	go build -ldflags "-X github.com/netlify/gotell/cmd.Version=`git rev-parse HEAD`"

deps: ## Install dependencies.
	@go get -u golang.org/x/lint/golint
	@go mod download

image: ## Build the Docker image.
	docker build .

lint: ## Lint the code
	golint `go list ./... | grep -v /vendor/`

test: ## Run tests.
	go test -v `go list ./... | grep -v /vendor/`
