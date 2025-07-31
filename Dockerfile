# Build stage
FROM golang:alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o sun .

# Final stage
FROM alpine:latest

WORKDIR /app

# Install dependencies including Helm
RUN apk add --no-cache ca-certificates curl && \
    curl -fsSL -o get_helm.sh https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 && \
    chmod 700 get_helm.sh && \
    ./get_helm.sh && \
    rm get_helm.sh

# Create non-root user
RUN adduser -D -g '' sun

# Copy the binary from builder
COPY --from=builder /app/sun /app/

# Set proper permissions
RUN chown -R sun:sun /app

# Switch to non-root user
USER sun

# Run the application
ENTRYPOINT ["/app/sun"] 