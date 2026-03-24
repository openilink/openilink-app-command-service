FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /command-service-bridge-app .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /command-service-bridge-app /command-service-bridge-app
EXPOSE 8081
CMD ["/command-service-bridge-app"]
