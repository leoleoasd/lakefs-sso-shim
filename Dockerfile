FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod .
RUN go mod download 2>/dev/null || true
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -buildvcs=false -o /shim .
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /shim /usr/local/bin/shim
EXPOSE 8088
ENTRYPOINT ["shim"]
