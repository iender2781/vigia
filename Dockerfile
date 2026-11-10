# Imagen de la plataforma de contexto de Vigía: un único binario estático con la web
# embebida y la ontología en /etc/contexto/ontology.

FROM node:22-alpine AS web
WORKDIR /src/web/contexto
COPY web/contexto/package.json web/contexto/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/contexto/ ./
RUN mkdir -p /src/internal/contexto/web/dist && npm run build

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/contexto/web/dist/ internal/contexto/web/dist/
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/contexto ./cmd/contexto \
 && /out/contexto ontology validate -dir ontology

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/contexto /usr/local/bin/contexto
COPY ontology/ /etc/contexto/ontology/
ENV CONTEXTO_ONTOLOGY_DIR=/etc/contexto/ontology
USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/contexto"]
CMD ["serve", "--role", "all"]
