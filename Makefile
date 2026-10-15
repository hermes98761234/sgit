export PATH := $(HOME)/.local/go/bin:$(HOME)/.local/protoc/bin:$(HOME)/go/bin:$(PATH)
MODULE := github.com/hermes98761234/sgit

.PHONY: gen test vet lint build docker smoke e2e

gen:
	protoc -I proto \
	  --go_out=. --go_opt=module=$(MODULE) \
	  --go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
	  proto/sgit/v1/*.proto

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	staticcheck ./...

build:
	CGO_ENABLED=0 go build -trimpath -o dist/ ./cmd/...

docker:
	docker build -t sgit-server:dev .

smoke:
	./scripts/docker-smoke.sh

e2e:
	./scripts/docker-e2e.sh
