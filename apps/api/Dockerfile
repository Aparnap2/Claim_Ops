# ClaimOps API — production image (Cloud Run contract).
# Alpine-only, squeezed: Go build -> minimal Alpine runtime, non-root,
# ca-certificates for TLS (GCS/Pub/Sub/Cloud SQL). Static binary, no shell
# tools in the final layer.
# Cloud Run sets PORT; the binary listens on :$PORT.
FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && ls -la /out/api

FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
 && adduser -D -H -u 10001 apprunner \
 && rm -rf /var/cache/apk/*
WORKDIR /app
COPY --from=build /out/api /app/api
ENV PORT=8080 APP_ENV=prod
EXPOSE 8080
USER apprunner
ENTRYPOINT ["/app/api"]
