# syntax=docker/dockerfile:1
# Build stage: compile a static binary.
# v0.4: golang 1.24 — modernc.org/sqlite requires a newer toolchain than 1.22.
FROM docker.io/library/golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cloud2api ./cmd/cloud2api

# Runtime stage.
FROM docker.io/library/alpine:3.20
COPY --from=build /out/cloud2api /usr/local/bin/cloud2api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cloud2api"]
