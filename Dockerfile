FROM golang:1.27.0-alpine3.23 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kitchen-api ./cmd/kitchen-api
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/example-establishment ./cmd/example-establishment

FROM alpine:3.23 AS runtime

RUN apk add --no-cache ca-certificates wget
WORKDIR /app
COPY --from=build /out/kitchen-api /app/kitchen-api
COPY --from=build /out/example-establishment /app/example-establishment
USER nobody
