# syntax=docker/dockerfile:1
# ClaimGraph backend. Stages: build -> (test | runtime).
# The module needs Go >= 1.26 (see go.mod).

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && go build -trimpath -ldflags="-s -w" -o /out/seed ./cmd/seed \
 && go build -trimpath -ldflags="-s -w" -o /out/seedscale ./cmd/seedscale

# `docker compose --profile test run --rm eval` runs the whole Go suite (including the T01-T30 evaluation)
# against the compose FalkorDB. Tests only create and delete their own claimgraph_test_* / claimgraph_eval_* graphs.
FROM build AS test
CMD ["go", "test", "-count=1", "./..."]

FROM alpine:3.22 AS runtime
RUN addgroup -S app && adduser -S -G app -H -s /sbin/nologin app
COPY --from=build /out/api /out/seed /out/seedscale /usr/local/bin/
USER app
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
CMD ["api"]
