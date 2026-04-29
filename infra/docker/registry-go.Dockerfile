# Go registry service image (F13).
#
# Multi-stage build: builder downloads modules + compiles, final stage is
# distroless so the image is ~10 MB.

FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go/go.mod go/go.sum ./
RUN go mod download

COPY go/ .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /registry ./cmd/registry

# ─── final stage ─────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /registry /registry

EXPOSE 8090
ENTRYPOINT ["/registry"]
