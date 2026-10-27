FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" \
    -o /out/bridge ./cmd/bridge

FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

RUN useradd --system --create-home --uid 10001 app

WORKDIR /app
COPY --from=build /out/bridge /app/bridge

RUN mkdir -p /app/data && chown -R app:app /app
USER app

ENTRYPOINT ["/app/bridge"]
