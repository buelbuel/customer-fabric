FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -o /api ./cmd/api
FROM scratch
COPY --from=build /api /api
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/api"]
