FROM golang:1.27.3-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /api ./cmd/api
FROM scratch
COPY --from=build /api /api
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/api"]
