ARG GO_VERSION=1.25
FROM golang:${GO_VERSION}-trixie AS build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/sprout \
    ./cmd/sprout

FROM debian:trixie-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        git \
        postgresql-client \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/sprout /usr/local/bin/sprout

ENTRYPOINT ["sprout"]
