FROM golang:1.15 AS builder

WORKDIR $GOPATH/src/github.com/ktrufanov/k8s-calico-networksets-controller
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /calico-networksets-controller calico-networksets-controller.go
FROM alpine:latest
COPY --from=builder /calico-networksets-controller /calico-networksets-controller

CMD ["./calico-networksets-controller"]
