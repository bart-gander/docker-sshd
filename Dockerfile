FROM golang:1.26-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/kube-sshd \
    ./cmd/kube-sshd

FROM alpine:3.22

RUN apk add --no-cache ca-certificates openssh-keygen \
    && mkdir -p /etc/ssh \
    && ssh-keygen -t ed25519 -N "" -f /etc/ssh/ssh_host_ed25519_key \
    && chmod 600 /etc/ssh/ssh_host_ed25519_key \
    && chmod 644 /etc/ssh/ssh_host_ed25519_key.pub

COPY --from=builder /out/kube-sshd /usr/local/bin/kube-sshd

EXPOSE 2232

ENTRYPOINT ["/usr/local/bin/kube-sshd"]
