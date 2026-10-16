include .env
export

DATABASE_URL=postgres://$(DB_USER):$(DB_PASSWORD)@$(DB_HOST):$(DB_PORT)/$(DB_NAME)?sslmode=$(DB_SSLMODE)

.PHONY: migrate-up migrate-down migrate-version test test-integration docker-up docker-down docker-reset docker-logs

migrate-up:
	migrate -path migrations -database "$(DATABASE_URL)" up

migrate-down:
	migrate -path migrations -database "$(DATABASE_URL)" down 1

migrate-version:
	migrate -path migrations -database "$(DATABASE_URL)" version

# Unit tests: domain, handler, service (in-memory fakes). No database needed.
test:
	go test -race ./...

# Integration/concurrency tests against PostgreSQL from .env (migrations applied).
test-integration:
	@# make exports .env values with their quotes; unset so the test loads .env itself.
	@unset APP_PORT APP_ENV DB_HOST DB_PORT DB_USER DB_PASSWORD DB_NAME DB_SSLMODE LOG_LEVEL; \
	go test -tags integration -race -count=1 ./internal/feature/wallet/integration/

# --- Docker: Postgres + migrations + seed data + API, all in one command ---------
# make exports .env values with their quotes, which would leak into docker compose;
# unset them so compose reads .env itself.
COMPOSE = unset APP_PORT APP_ENV DB_HOST DB_PORT DB_USER DB_PASSWORD DB_NAME DB_SSLMODE LOG_LEVEL; docker compose

docker-up:
	@$(COMPOSE) up -d --build --wait

docker-down:
	@$(COMPOSE) down

# Wipe the database volume and start again with a fresh DB, migrations and seed data.
docker-reset:
	@$(COMPOSE) down -v
	@$(COMPOSE) up -d --build --wait

docker-logs:
	@$(COMPOSE) logs -f api
