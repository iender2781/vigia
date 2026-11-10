# Tareas habituales del monorepo de Vigía (plataforma de contexto).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
IMAGE   ?= ghcr.io/iender2781/vigia-contexto
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: all web build test test-integration equivalence contracts ontology helm-check image dev dev-down clean

all: web build test

web:                     ## Compila la web (React) dentro del paquete Go
	cd web/contexto && npm ci --no-audit --no-fund && npm run build

build:                   ## Compila el binario en bin/contexto
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/contexto ./cmd/contexto

test:                    ## Tests unitarios (sin base de datos)
	go vet ./...
	go test ./...

test-integration:        ## Tests con PostgreSQL: CONTEXTO_TEST_DATABASE_URL=postgres://... make test-integration
	@test -n "$(CONTEXTO_TEST_DATABASE_URL)" || (echo "define CONTEXTO_TEST_DATABASE_URL" && exit 1)
	go test -count=1 ./internal/contexto/store/...

equivalence:             ## Compara la CMDB con el correlador Python (CAPTURA=dir opcional)
	tools/reference/equivalence.sh $(CAPTURA)

ontology:                ## Valida la ontología del repositorio
	go run ./cmd/contexto ontology summary -dir ontology

helm-check:              ## Renderiza y valida el chart sin Helm (en CI se usa helm lint)
	go run ./tools/helmcheck deploy/helm/vigia-contexto

image:                   ## Construye la imagen de contenedor
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(VERSION) .

dev:                     ## Entorno local: PostgreSQL + Prometheus simulado + plataforma (http://localhost:8080)
	docker compose -f deploy/compose/docker-compose.yml up --build -d

dev-down:
	docker compose -f deploy/compose/docker-compose.yml down -v

clean:
	rm -rf bin internal/contexto/web/dist/assets internal/contexto/web/dist/index.html
