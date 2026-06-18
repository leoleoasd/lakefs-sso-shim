# Builds the lakeFS reference ACL server from contrib/ (no official image exists).
# This is what gives OSS lakeFS multi-user + group management via the auth API.
FROM golang:1.25 AS build
RUN git clone --depth 1 https://github.com/treeverse/lakeFS.git /src
WORKDIR /src
RUN CGO_ENABLED=0 go build -o /aclserver ./contrib/auth/acl/cmd/acl
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /aclserver /usr/local/bin/aclserver
EXPOSE 8001
ENTRYPOINT ["aclserver"]
CMD ["run"]
