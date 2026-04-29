# Go gateway service image (F06).
#
# Multi-stage build: builder downloads modules + compiles, final stage is
# distroless so the image is ~10 MB instead of ~300 MB.

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go/go.mod go/go.sum ./
RUN go mod download

COPY go/ .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /gateway ./cmd/gateway

# ─── final stage ─────────────────────────────────────────────────────────────
FROM alpine:3.21

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /gateway /gateway

EXPOSE 8080
ENTRYPOINT ["/gateway"]
