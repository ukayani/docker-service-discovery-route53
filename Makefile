# Tell make to treat all targets as phony
.PHONY: all build test clean run vet install build-docker release-ecr

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
build-docker: install
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

release-dockerhub: build-docker
	docker push $(DOCKER_REPO):latest