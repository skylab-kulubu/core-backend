.PHONY: data-up data-down

data-up:
	docker compose -f deploy/compose.yaml up -d

data-down:
	docker compose -f deploy/compose.yaml down
