# Go orchestrator service image (F01).
#
# Multi-stage build: builder downloads modules + compiles, final stage is
# distroless so the image is ~10 MB instead of ~300 MB.

FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go/go.mod go/go.sum ./
RUN go mod download

COPY go/ .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /orchestrator ./cmd/orchestrator

# ─── final stage ─────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /orchestrator /orchestrator

EXPOSE 8081
ENTRYPOINT ["/orchestrator"]
