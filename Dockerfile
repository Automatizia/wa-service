FROM golang:1.23-alpine AS builder

RUN apk add --no-cache gcc musl-dev git

WORKDIR /app
COPY go.mod ./
RUN go mod download && go mod tidy 2>/dev/null || true

COPY . .
RUN CGO_ENABLED=0 go build -o wa-service .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/wa-service .

VOLUME ["/data"]
EXPOSE 3000

CMD ["./wa-service"]
