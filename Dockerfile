# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o icd-converter .

# CA certificates (needed for HTTPS calls to LLM endpoints)
FROM alpine:3 AS certs
RUN apk --no-cache add ca-certificates

# Production image
FROM scratch

WORKDIR /app
COPY --from=builder /app/icd-converter .
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 8080

# Mount a volume at /data for DB persistence: -v icd_data:/data -e ICD_DB_PATH=/data/icd.db
ENTRYPOINT ["/app/icd-converter"]