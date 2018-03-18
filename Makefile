# Tell make to treat all targets as phony
.PHONY: all build test clean run vet install build-linux release-ecr

# Go parameters
BINARY_NAME=service-discovery-agent
BINARY_UNIX=$(BINARY_NAME)-unix
PROJECT=github.com/ukayani/docker-service-discovery-route53
GO_VERSION=1.10
DOCKER_REPO=kayaniu/service-discovery-agent-route53

all: install build

build:
	@go build -o $(BINARY_NAME) -v

# Cross compilation
build-linux: install
	docker run -e GOPATH=/app/ \
		-e CGO_ENABLED=0 \
		--rm  \
		-v "$(PWD):/target" \
		-v "$(PWD):/app/src/$(PROJECT)" \
		-w /target  \
		golang:$(GO_VERSION) \
		go build \
		-o $(BINARY_UNIX) \
		$(PROJECT)
	 docker build -t $(DOCKER_REPO):latest .

test:
	@go test -v ./...

install:
	dep ensure
clean:
	@go clean
	rm -f $(BINARY_NAME)
	rm -f $(BINARY_UNIX)

vet:
	@go vet


release-ecr: build-linux
	eval $(aws ecr get-login --no-include-email)
	docker push $(DOCKER_REPO):latest

release-dockerhub: build-linux
	docker push $(DOCKER_REPO):latest