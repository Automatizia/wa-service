FROM golang:1.23-alpine AS builder

ENV GONOSUMDB=* GOPROXY=https://proxy.golang.org,direct

RUN apk add --no-cache git

WORKDIR /app
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o wa-service .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/wa-service .

VOLUME ["/data"]
EXPOSE 3000

CMD ["./wa-service"]
