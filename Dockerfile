# helm: .containers.main
FROM golang:1.21.1-alpine3.18 as builder

ARG GOPROXY

RUN apk add --no-cache git && rm -rf /var/cache/apk/*

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .

WORKDIR /app
RUN export LOCAL_DEV=NOT
RUN CGO_ENABLED=0 go build -o service

FROM gcr.io/kaniko-project/executor:v1.16.0 AS final

WORKDIR /app
COPY --from=builder /app/service .

ENTRYPOINT ["./service"]
