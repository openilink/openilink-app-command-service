FROM golang:1.26-alpine AS builder
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o /command-service-app .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /command-service-app /command-service-app
EXPOSE 8081
CMD ["/command-service-app"]
