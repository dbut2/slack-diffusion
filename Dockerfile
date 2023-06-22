FROM golang:alpine AS builder

WORKDIR /app

COPY ./go.mod ./go.mod
COPY ./go.sum ./go.sum

COPY ./apigen.go ./apigen.go
COPY ./functions.go ./functions.go
COPY ./main.go ./main.go

RUN go build -o diffusion

FROM alpine

COPY --from=builder /app/diffusion ./diffusion

CMD ["./diffusion"]

