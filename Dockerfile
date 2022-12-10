# Start by building the application.
FROM golang:1.18 as build

WORKDIR /go/src/app
COPY . .

RUN go mod download
RUN export GOOS=linux && \
    export GOARCH=amd64 && \
    export GOPROXY=https://proxy.golang.org,direct && \
    export CGO_ENABLED=0 && \
    go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /go/bin/goandlead && \
    touch -t 202002020000 cdk-sops-secrets && \
    chmod 755 cdk-sops-secrets

FROM gcr.io/distroless/static-debian11
COPY --from=build /go/bin/goandlead /
CMD ["/goandlead"]