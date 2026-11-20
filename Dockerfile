# syntax=docker/dockerfile:1
FROM golang:1.27.4-bookworm AS build

WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN go build -trimpath -ldflags="-s -w -buildid=" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gateway /gateway

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/gateway"]
