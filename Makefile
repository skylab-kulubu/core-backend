.PHONY: data-up data-down run test

data-up:
	docker compose -f deploy/compose.yaml up -d

data-down:
	docker compose -f deploy/compose.yaml down

run:
	go run ./cmd/core-backend

test:
	go test ./...
