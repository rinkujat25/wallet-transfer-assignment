include .env
export

DATABASE_URL=postgres://$(DB_USER):$(DB_PASSWORD)@$(DB_HOST):$(DB_PORT)/$(DB_NAME)?sslmode=$(DB_SSLMODE)

.PHONY: migrate-up migrate-down migrate-version test test-integration

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
