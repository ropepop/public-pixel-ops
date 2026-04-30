.PHONY: run test build tidy spacetime-install spacetime-build docker-image-build clean-local

SPACETIME ?= $(HOME)/.local/bin/spacetime

run:
	TICKET_REMOTE_AUTH_MODE=dev go run ./cmd/ticket-remote

test:
	go test ./...

build:
	go build -o ./bin/ticket-remote ./cmd/ticket-remote

tidy:
	go mod tidy

spacetime-install:
	cd ./spacetimedb && npm install

spacetime-build:
	cd ./spacetimedb && HOME=$(HOME) npm run build

docker-image-build:
	docker build -f ../../infra/arbuzas/docker/images/ticket-remote.Dockerfile -t arbuzas/ticket-remote:dev ../../

clean-local:
	rm -rf ./bin ./spacetimedb/node_modules ./spacetimedb/dist

