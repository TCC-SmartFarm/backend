# Compilação
FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o sensor-simulator .

# Execução
FROM alpine:latest
# tzdata para o ciclo dia/noite respeitar TZ=America/Sao_Paulo
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /root/
COPY --from=builder /app/sensor-simulator .

CMD ["./sensor-simulator"]
