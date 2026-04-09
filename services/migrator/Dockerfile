# --- Build stage ---
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /migrator ./cmd/server

# --- Runtime stage ---
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /migrator /migrator

EXPOSE 8090

USER nonroot:nonroot
ENTRYPOINT ["/migrator"]
