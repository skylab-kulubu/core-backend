.PHONY: data-up data-down stack-up run test

data-up:
	docker compose -f deploy/compose.yaml up -d postgres redis

stack-up:
	docker compose -f deploy/compose.yaml --profile core up -d --build

data-down:
	docker compose -f deploy/compose.yaml --profile core down

run:
	go run ./cmd/core-backend

test:
	go test ./...
