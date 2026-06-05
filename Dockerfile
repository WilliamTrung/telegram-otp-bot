FROM golang:1.25-alpine AS builder

# Install system dependencies if needed
RUN apk --no-cache add git

WORKDIR /app

# Copy module files and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application files
COPY . .

# Compile the Go application statically and strip debug symbols (-s -w)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o bot .

# Final minimal production container
FROM alpine:latest

# Install CA certificates and timezone data for TLS connections
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy the statically compiled binary from the builder stage
COPY --from=builder /app/bot /app/bot

# Expose HTTP port
EXPOSE 8080

# Run the binary
CMD ["/app/bot"]
