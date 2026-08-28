GO_FILES := $(shell find . -type f -name '*.go' -not -path './internal/contract/*')
LINT_IMAGE := golangci/golangci-lint:v2.13.1
PLANTUML_IMAGE := plantuml/plantuml:1.2025.10
POSTGRES_PORT ?= 5432
INTEGRATION_DATABASE := avito_kitchen_integration
TEST_DATABASE_URL := postgres://avito_kitchen:avito_kitchen@localhost:$(POSTGRES_PORT)/$(INTEGRATION_DATABASE)?sslmode=disable

.PHONY: fmt fmt-check tidy-check generate generate-check test test-race vet test-integration lint openapi-check diagrams diagrams-check compose-check test-e2e verify

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@test -z "$$(gofmt -l $(GO_FILES))"

tidy-check:
	@tmp=$$(mktemp -d); cp go.mod go.sum $$tmp/; go mod tidy; status=$$?; if [ $$status -eq 0 ]; then cmp $$tmp/go.mod go.mod && cmp $$tmp/go.sum go.sum; status=$$?; fi; find $$tmp -type f -delete; rmdir $$tmp; exit $$status

generate:
	go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml

generate-check:
	@tmp=$$(mktemp); cp internal/contract/api.gen.go $$tmp; $(MAKE) generate >/dev/null; status=$$?; if [ $$status -eq 0 ]; then cmp $$tmp internal/contract/api.gen.go; status=$$?; fi; find $$tmp -delete; exit $$status

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

test-integration:
	docker compose up -d --wait db
	docker compose exec -T db dropdb --if-exists -U avito_kitchen $(INTEGRATION_DATABASE)
	docker compose exec -T db createdb -U avito_kitchen $(INTEGRATION_DATABASE)
	docker compose run --rm --no-deps migrate -path=/migrations "-database=postgres://avito_kitchen:avito_kitchen@db:5432/$(INTEGRATION_DATABASE)?sslmode=disable" up
	docker compose run --rm --no-deps seed psql -v ON_ERROR_STOP=1 -h db -U avito_kitchen -d $(INTEGRATION_DATABASE) -f /seed/demo.sql
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -count=1 -tags=integration ./tests/integration

lint:
	docker run --rm -v "$(CURDIR):/app" -w /app $(LINT_IMAGE) golangci-lint run

openapi-check:
	$(MAKE) generate-check

diagrams:
	docker run --rm -v "$(CURDIR)/docs/diagrams:/data" $(PLANTUML_IMAGE) -tsvg -o generated /data/*.puml

diagrams-check:
	@tmp=$$(mktemp -d); cp docs/diagrams/generated/*.svg $$tmp/; $(MAKE) diagrams >/dev/null; status=$$?; if [ $$status -eq 0 ]; then diff -ru $$tmp docs/diagrams/generated; status=$$?; fi; find $$tmp -type f -delete; rmdir $$tmp; exit $$status

compose-check:
	docker compose config --quiet

test-e2e:
	docker compose up -d --build --wait --force-recreate kitchen-api example-establishment
	KITCHEN_API_URL=http://localhost:$${KITCHEN_PORT:-8080} ESTABLISHMENT_API_URL=http://localhost:$${ESTABLISHMENT_PORT:-8081} go test -count=1 -tags=e2e ./tests/e2e

verify:
	$(MAKE) fmt-check
	$(MAKE) tidy-check
	$(MAKE) openapi-check
	$(MAKE) test
	$(MAKE) test-race
	$(MAKE) vet
	$(MAKE) lint
	$(MAKE) diagrams-check
	$(MAKE) compose-check
	$(MAKE) test-integration
	$(MAKE) test-e2e
