-include .env

APP_NAME ?= cronsole
APP_PORT ?= 3333
DEV_DB_PORT ?= 55433
VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

# The network the application and the Docker API proxy share.
#
# It has to be a network created here rather than the default bridge: the
# default bridge does not resolve container names, so DOCKER_API pointing at the
# proxy by name would never connect. Everything else about the container is
# unchanged, published ports included.
NETWORK ?= $(APP_NAME)-net

# The read-only Docker API proxy behind the container panel. It holds the
# socket so this application never has to; see docs/operations.md.
PROXY_NAME := $(APP_NAME)-dockerproxy
PROXY_IMAGE ?= tecnativa/docker-socket-proxy:v0.5.0

# The repository tests get their OWN database, on its own port and with a fixed
# name rather than one derived from APP_NAME. Two reasons: they truncate every
# table between cases, so pointing them at the development database would delete
# whatever is being worked on, and a name that follows APP_NAME would move the
# moment somebody renames the application.
TEST_DB_NAME := cronsole-test-db
TEST_DB_PORT ?= 55434
TEST_DB_URL := postgres://cronsole:cronsole@localhost:$(TEST_DB_PORT)/cronsole_test?sslmode=disable

.DEFAULT_GOAL := help

.PHONY: help build cli run live test test-repo cover lint dev-db dev-schema dev-stop \
	test-db test-db-stop docker-network docker-build docker-run docker-stop \
	docker-proxy docker-proxy-stop clean-image

## help: List the available commands
help: makefile
	@echo
	@echo " Commands"
	@echo
	@sed -n 's/^##//p' $< | column -t -s ':' | sed -e 's/^/ /'
	@echo

## build: Compile the server into ./cronsole
build:
	@go build -ldflags "-X main.version=$(VERSION)" -o cronsole ./cmd/cronsole

## cli: Compile the command line client into ./cronsolectl
cli:
	@go build -ldflags "-X main.version=$(VERSION)" -o cronsolectl ./cmd/cronsolectl

## run: Build and run against the settings in .env
run: build
	@./cronsole

## live: Rebuild and restart on every change (needs entr)
## live: Templates and static files are embedded, so a change to either needs this rebuild.
live:
	@find . -type f \( -name '*.go' -o -name '*.gohtml' -o -name '*.css' -o -name '*.js' \) \
		-not -path './.git/*' | entr -r sh -c 'go build -o /tmp/$(APP_NAME) ./cmd/cronsole && ENV_FILE=$${ENV_FILE:-.env} /tmp/$(APP_NAME)'

## test: Run the test suite
test:
	@go test ./...

## test-repo: Run the repository tests against the test database
## test-repo: Starts it if it is not up. These are the only tests that need Postgres.
test-repo: test-db
	@CRONSOLE_TEST_DB="$(TEST_DB_URL)" go test ./internal/repository/... -count=1

## cover: Coverage, measured exactly the way CI measures it
## cover: Same packages, same flags, same 85% floor. It used to differ and the
## cover: local figure was the one nobody could reproduce.
cover:
	@pkgs=$$(go list ./... | grep -v '/internal/repository/memrepo$$' | paste -sd, -); \
	go test -coverpkg="$$pkgs" -coverprofile=/tmp/$(APP_NAME).cover ./... >/dev/null
	@go tool cover -func=/tmp/$(APP_NAME).cover | tail -n 25
	@pct=$$(go tool cover -func=/tmp/$(APP_NAME).cover | awk '/^total:/ {print substr($$3, 1, length($$3)-1)}'); \
	awk -v p="$$pct" 'BEGIN { exit (p >= 85) ? 0 : 1 }' \
		&& echo "total $$pct% (floor 85%)" \
		|| { echo "total $$pct% is BELOW the 85% floor"; exit 1; }

## lint: Vet and format check
lint:
	@go vet ./cmd/... ./internal/... ./pkg/...
	@test -z "$$(gofmt -l cmd internal pkg)" || (echo "gofmt needed:"; gofmt -l cmd internal pkg; exit 1)

## dev-db: Start a throwaway Postgres for local work, on port $(DEV_DB_PORT)
dev-db:
	@docker start $(APP_NAME)-dev-db 2>/dev/null || docker run -d \
		--name $(APP_NAME)-dev-db \
		-p $(DEV_DB_PORT):5432 \
		-e POSTGRES_USER=$(APP_NAME) \
		-e POSTGRES_PASSWORD=$(APP_NAME) \
		-e POSTGRES_DB=cron \
		postgres:17
	@echo "waiting for postgres"
	@until docker exec $(APP_NAME)-dev-db pg_isready -U $(APP_NAME) >/dev/null 2>&1; do sleep 1; done
	@echo "ready on localhost:$(DEV_DB_PORT)"

## dev-schema: Apply cronsole.sql to the local development database
dev-schema:
	@docker exec -i $(APP_NAME)-dev-db psql -U $(APP_NAME) -d cron -v ON_ERROR_STOP=1 < cronsole.sql >/dev/null
	@echo "schema applied"

## dev-stop: Stop the local development database
dev-stop:
	@docker stop $(APP_NAME)-dev-db >/dev/null 2>&1 || true
	@echo "stopped"

## test-db: Start the database the repository tests run against, on port $(TEST_DB_PORT)
## test-db: Separate from dev-db on purpose: the tests empty every table.
test-db:
	@docker start $(TEST_DB_NAME) >/dev/null 2>&1 || docker run -d \
		--name $(TEST_DB_NAME) \
		-p $(TEST_DB_PORT):5432 \
		-e POSTGRES_USER=cronsole \
		-e POSTGRES_PASSWORD=cronsole \
		-e POSTGRES_DB=cronsole_test \
		postgres:17 >/dev/null
	@until docker exec $(TEST_DB_NAME) pg_isready -U cronsole >/dev/null 2>&1; do sleep 1; done
	@docker exec -i $(TEST_DB_NAME) psql -U cronsole -d cronsole_test -v ON_ERROR_STOP=1 \
		< cronsole.sql >/dev/null
	@echo "test database ready on localhost:$(TEST_DB_PORT)"

## test-db-stop: Stop the test database
test-db-stop:
	@docker stop $(TEST_DB_NAME) >/dev/null 2>&1 || true
	@echo "stopped"

## docker-build: Build the container image
docker-build:
	@docker build --build-arg VERSION=$(VERSION) -t $(APP_NAME) .

## docker-network: Create the network the application and the proxy share
docker-network:
	@docker network inspect $(NETWORK) >/dev/null 2>&1 || docker network create $(NETWORK) >/dev/null
	@echo "network $(NETWORK) ready"

# The log options are not optional. Docker's default json-file driver never
# rotates, and with --restart always the container's stdout fills the disk of
# the machine it was installed to protect.
## docker-run: Run the container image with .env
docker-run: docker-build docker-stop docker-network
	@docker run -d \
		--env-file .env \
		--restart always \
		--name $(APP_NAME) \
		--network $(NETWORK) \
		-p $(APP_PORT):$(APP_PORT) \
		--log-opt max-size=10m \
		--log-opt max-file=3 \
		$(APP_NAME)

## docker-stop: Stop and remove the container
docker-stop:
	@docker rm -f $(APP_NAME) >/dev/null 2>&1 || true

# --privileged is upstream's own instruction: SELinux and AppArmor block the
# socket connection without it. It grants this container nothing it does not
# already have, because holding that socket is root on the host either way.
# Drop it on a host that enforces neither.
## docker-proxy: Run the read-only Docker API proxy the container panel reads
## docker-proxy: It holds the socket so Cronsole never does, and exposes GET of the container list alone.
docker-proxy: docker-network docker-proxy-stop
	@docker run -d \
		--name $(PROXY_NAME) \
		--network $(NETWORK) \
		--restart always \
		--privileged \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-e CONTAINERS=1 \
		--log-opt max-size=10m \
		--log-opt max-file=3 \
		$(PROXY_IMAGE)
	@echo "proxy running. Put this in .env, then run make docker-run:"
	@echo "  DOCKER_API=http://$(PROXY_NAME):2375"

## docker-proxy-stop: Stop and remove the Docker API proxy
docker-proxy-stop:
	@docker rm -f $(PROXY_NAME) >/dev/null 2>&1 || true

## clean-image: Remove the built image and dangling layers
clean-image:
	@docker rmi $(APP_NAME) 2>/dev/null || true
	@docker image prune -f >/dev/null
